package control

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"panel-agent.local/panel-agent/internal/agent"
	"panel-agent.local/panel-agent/internal/security"
)

func TestBackupExportRestoreInvalidatesSessionAndRestoresSQLiteGeneration(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := "backup-admin-password"
	hash, err := security.HashAdminPassword([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dataDir, hash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		_ = server.Close()
	})
	client, baseURL := newTestClient(t, httpServer.URL)
	loginControlAdmin(t, client, baseURL, password)
	csrf := csrfCookie(t, client, baseURL)
	response := postForm(t, client, baseURL+"/admin/backup/export", url.Values{"csrf": {csrf}, "password": {"bundle-password"}}, baseURL)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("backup export status = %d: %s", response.StatusCode, body)
	}
	backupBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || len(backupBytes) == 0 {
		t.Fatalf("backup export body = %d bytes, err=%v", len(backupBytes), err)
	}
	csrf = csrfCookie(t, client, baseURL)
	response = postForm(t, client, baseURL+"/admin/settings", url.Values{"csrf": {csrf}, "site_name": {"mutated after backup"}}, baseURL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings mutation status = %d", response.StatusCode)
	}
	csrf = csrfCookie(t, client, baseURL)
	badResponse := postMultipartBackup(t, client, baseURL+"/admin/backup/restore", backupBytes, "wrong-password", csrf, baseURL)
	if badResponse.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(badResponse.Body)
		badResponse.Body.Close()
		t.Fatalf("wrong-password restore status = %d: %s", badResponse.StatusCode, body)
	}
	badResponse.Body.Close()
	csrf = csrfCookie(t, client, baseURL)
	restoreResponse := postMultipartBackup(t, client, baseURL+"/admin/backup/restore", backupBytes, "bundle-password", csrf, baseURL)
	if restoreResponse.StatusCode != http.StatusSeeOther || !strings.Contains(restoreResponse.Header.Get("Location"), "restored=1") {
		body, _ := io.ReadAll(restoreResponse.Body)
		restoreResponse.Body.Close()
		t.Fatalf("restore status = %d location=%q: %s", restoreResponse.StatusCode, restoreResponse.Header.Get("Location"), body)
	}
	restoreResponse.Body.Close()
	response = request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("restore retained an old admin session: %d", response.StatusCode)
	}
	response.Body.Close()
	loginControlAdmin(t, client, baseURL, password)
	response = request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("settings after restore status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Contains(body, []byte(`"site_name":"Panel-Agent"`)) {
		t.Fatalf("restored settings body = %s, err=%v", body, err)
	}
}

func postMultipartBackup(t *testing.T, client *http.Client, endpoint string, contents []byte, password, csrf, origin string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("csrf", csrf)
	_ = writer.WriteField("password", password)
	file, err := writer.CreateFormFile("backup", "panel-agent-backup.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestBackupRestoreForcesAgentFullSnapshotReconnect(t *testing.T) {
	dataDir := t.TempDir()
	_ = os.Chmod(dataDir, 0o700)
	password := "backup-agent-admin-password"
	hash, err := security.HashAdminPassword([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dataDir, hash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		_ = server.Close()
	})
	adminClient, _ := newTestClient(t, httpServer.URL)
	adminClient.Transport = httpServer.Client().Transport
	loginControlAdmin(t, adminClient, httpServer.URL, password)
	caPath := filepath.Join(t.TempDir(), "panel-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: httpServer.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	csrf := fetchMachinesCSRF(t, adminClient, httpServer.URL)
	joinToken := createJoinTokenThroughAdmin(t, adminClient, httpServer.URL, csrf, "")
	agentDir := filepath.Join(t.TempDir(), "agent")
	machineID, err := agent.Pair(context.Background(), agentDir, httpServer.URL, "restore agent", joinToken, caPath, false)
	if err != nil {
		t.Fatal(err)
	}
	agentClient, err := agent.LoadClient(agentDir, caPath)
	if err != nil {
		t.Fatal(err)
	}
	defer agentClient.Close()
	oldAuthority := agentClient.Identity().AuthorityEpoch
	csrf = csrfCookie(t, adminClient, httpServer.URL)
	response := postForm(t, adminClient, httpServer.URL+"/admin/backup/export", url.Values{"csrf": {csrf}, "password": {"agent-backup-password"}}, httpServer.URL)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("agent backup export status = %d: %s", response.StatusCode, body)
	}
	backupBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	csrf = csrfCookie(t, adminClient, httpServer.URL)
	response = postMultipartBackup(t, adminClient, httpServer.URL+"/admin/backup/restore", backupBytes, "agent-backup-password", csrf, httpServer.URL)
	if response.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("agent backup restore status = %d: %s", response.StatusCode, body)
	}
	response.Body.Close()
	if err := agentClient.SyncOnce(context.Background()); err != nil {
		t.Fatalf("Agent did not reconnect after Panel restore: %v", err)
	}
	identity := agentClient.Identity()
	if identity.MachineID != machineID || identity.AuthorityEpoch == oldAuthority || identity.AuthorityNonce != "" || identity.Digest == "" {
		t.Fatalf("Agent identity after restore = %+v", identity)
	}
}
