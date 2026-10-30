package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestExtensionImportEnableDisableAndDeletePage(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashAdminPassword([]byte("d13-extension-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dir, hash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewTLSServer(server.Handler())
	defer httpServer.Close()
	admin := httpServer.Client()
	admin.Jar, _ = cookiejar.New(nil)
	admin.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	loginControlAdmin(t, admin, httpServer.URL, "d13-extension-password")
	csrf := fetchMachinesCSRF(t, admin, httpServer.URL)
	packageJSON, err := json.Marshal(testExtensionPackage())
	if err != nil {
		t.Fatal(err)
	}
	archive := zipTestPackage(t, packageJSON)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("package", "trojan-tls-v1.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/extensions/import", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", httpServer.URL)
	response, err := admin.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("extension import returned HTTP %d", response.StatusCode)
	}
	page, err := admin.Get(httpServer.URL + "/admin/extensions")
	if err != nil {
		t.Fatal(err)
	}
	pageHTML, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !bytes.Contains(pageHTML, []byte("trojan-tls-test")) || !bytes.Contains(pageHTML, []byte("导入并静态验证")) {
		t.Fatalf("extension page did not render imported package: %d %s", page.StatusCode, pageHTML)
	}
	ctx, now := context.Background(), time.Now().UTC()
	join, _ := security.NewToken(security.MachineSecretBytes)
	candidate, _ := security.NewToken(security.MachineSecretBytes)
	registration, _ := security.NewUUID()
	if err := server.store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := server.store.RegisterMachine(ctx, storage.RegisterMachineInput{RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate), Hostname: "d13-extension-host", OS: "linux", Architecture: "amd64", AgentVersion: protocol.AgentVersion}, now)
	if err != nil {
		t.Fatal(err)
	}
	nodesPage, err := admin.Get(httpServer.URL + "/admin/machines/" + machine.MachineID + "/nodes")
	if err != nil {
		t.Fatal(err)
	}
	nodesHTML, _ := io.ReadAll(nodesPage.Body)
	nodesPage.Body.Close()
	if nodesPage.StatusCode != http.StatusOK || !bytes.Contains(nodesHTML, []byte("extension_trojan-tls-test_1.0.0_sni")) || !bytes.Contains(nodesHTML, []byte("Trojan TLS test")) {
		t.Fatalf("Node page did not render extension schema fields: %d %s", nodesPage.StatusCode, nodesHTML)
	}
	for _, action := range []string{"enable", "disable"} {
		response := postForm(t, admin, httpServer.URL+"/admin/extensions/trojan-tls-test/1.0.0/"+action, url.Values{"csrf": {csrf}}, httpServer.URL)
		response.Body.Close()
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("extension %s returned HTTP %d", action, response.StatusCode)
		}
	}
	response = postForm(t, admin, httpServer.URL+"/admin/extensions/trojan-tls-test/1.0.0/delete", url.Values{"csrf": {csrf}}, httpServer.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("extension delete returned HTTP %d", response.StatusCode)
	}
}

func testExtensionPackage() extensions.Package {
	return extensions.Package{
		Manifest:  extensions.Manifest{ID: "trojan-tls-test", Version: "1.0.0", Name: "Trojan TLS test", Description: "fixed test package", Core: extensions.CoreSupport{Kind: "xray", MinVersion: "26.10.21"}, Capabilities: extensions.Capabilities{Direct: true, URI: true, Mihomo: true, Certificate: true}},
		Schema:    extensions.Schema{Fields: []extensions.Field{{Name: "sni", Type: "host", Label: "TLS SNI", Required: true, Max: 253}, {Name: "certificate_pem", Type: "pem", Label: "TLS 证书 PEM", Required: true, Secret: true, Max: 65536}, {Name: "private_key_pem", Type: "private_key_pem", Label: "TLS 私钥 PEM", Required: true, Secret: true, Max: 65536}}},
		Renderers: extensions.Renderers{Xray: extensions.XrayRenderer{Kind: "trojan_tls", Protocol: "trojan", Network: "tcp", TLS: true}, URI: extensions.SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}, Mihomo: extensions.SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}},
	}
}

func zipTestPackage(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	part, err := writer.Create(extensions.PackageFileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
