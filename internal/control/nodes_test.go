package control

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"panel-agent.local/panel-agent/internal/agent"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestNodeAdminFormsMachineScopeAndRepair(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := "d04-node-control-password"
	hash, err := security.HashAdminPassword([]byte(password))
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
	loginControlAdmin(t, admin, httpServer.URL, password)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: httpServer.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	csrf := fetchMachinesCSRF(t, admin, httpServer.URL)
	pair := func() (string, string) {
		t.Helper()
		join := createJoinTokenThroughAdmin(t, admin, httpServer.URL, csrf, "")
		agentDir := filepath.Join(t.TempDir(), "agent")
		id, err := agent.Pair(context.Background(), agentDir, httpServer.URL, "test machine", join, caPath, false)
		if err != nil {
			t.Fatal(err)
		}
		return id, agentDir
	}
	a, agentDirA := pair()
	b, agentDirB := pair()
	exitForm := url.Values{
		"csrf": {csrf}, "public_host": {"exit.example.test"}, "public_port": {"45100"},
		"listen_host": {"127.0.0.1"}, "listen_port": {"19443"}, "server_name": {"www.cloudflare.com"}, "target": {"www.cloudflare.com:443"},
	}
	response := postForm(t, admin, httpServer.URL+"/admin/machines/"+b+"/exit", exitForm, httpServer.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("Exit form returned %d", response.StatusCode)
	}
	pageURL := httpServer.URL + "/admin/machines/" + a + "/nodes"
	page, err := admin.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	page.Body.Close()
	developmentCheckbox := regexp.MustCompile(`<input[^>]*name="development"[^>]*>`).Find(html)
	if page.StatusCode != http.StatusOK || len(developmentCheckbox) == 0 || bytes.Contains(developmentCheckbox, []byte("checked")) {
		t.Fatal("new Node form did not keep development credentials disabled")
	}
	form := url.Values{
		"csrf": {csrf}, "name": {"Reality A"}, "public_host": {"node.example.test"}, "public_port": {"45001"},
		"listen_host": {"127.0.0.1"}, "listen_port": {"18443"}, "server_name": {"www.cloudflare.com"}, "target": {"www.cloudflare.com:443"},
		"enabled": {"true"}, "development": {"true"}, "exit_machine_id": {b},
	}
	response = postForm(t, admin, pageURL, form, httpServer.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("Node form returned %d", response.StatusCode)
	}
	guidePage, err := admin.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	guideHTML, _ := io.ReadAll(guidePage.Body)
	guidePage.Body.Close()
	if guidePage.StatusCode != http.StatusOK || !strings.Contains(guidePage.Header.Get("Content-Security-Policy"), "connect-src 'self'") || !bytes.Contains(guideHTML, []byte("/assets/connectivity.js")) || !bytes.Contains(guideHTML, []byte("公网接入引导与有限检查")) || !bytes.Contains(guideHTML, []byte("data-copy-target")) {
		t.Fatal("Node page did not render the contextual copy/check guide")
	}
	apiURL := httpServer.URL + "/api/admin/machines/" + a + "/nodes"
	response, err = admin.Get(apiURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var listing struct {
		Nodes []storage.NodeRecord `json:"nodes"`
	}
	if err := json.Unmarshal(body, &listing); err != nil || len(listing.Nodes) != 1 || bytes.Contains(body, []byte("private_key")) {
		t.Fatalf("invalid or secret-bearing Node response: %v", err)
	}
	node := listing.Nodes[0]
	if node.PublicKey == "" || node.ShortID == "" || node.DevelopmentUUID == "" {
		t.Fatal("Node registration did not generate Reality and explicit development credentials")
	}
	mutate := func(path, token string, input []byte) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodPut, path, bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", httpServer.URL)
		request.Header.Set("X-CSRF-Token", token)
		response, err := admin.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	input, _ := json.Marshal(node.NodeInput)
	if status := mutate(apiURL+"/"+node.NodeID, "", input); status != http.StatusForbidden {
		t.Fatalf("Node edit without CSRF = %d", status)
	}
	otherPath := httpServer.URL + "/api/admin/machines/" + b + "/nodes/" + node.NodeID
	if status := mutate(otherPath, csrf, input); status != http.StatusNotFound {
		t.Fatalf("cross-machine Node reference = %d", status)
	}
	withMachineID := append([]byte(`{"machine_id":"`+b+`",`), input[1:]...)
	if status := mutate(apiURL+"/"+node.NodeID, csrf, withMachineID); status != http.StatusBadRequest {
		t.Fatalf("body-supplied machine identity accepted: %d", status)
	}
	clientA, err := agent.LoadClient(agentDirA, caPath)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, apiURL, nil)
	request.Header.Set("Authorization", "Bearer "+clientA.Identity().MachineSecret)
	machineHTTP := &http.Client{Transport: httpServer.Client().Transport}
	response, err = machineHTTP.Do(request)
	if err != nil {
		clientA.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		clientA.Close()
		t.Fatal("machine secret accessed Node admin API")
	}
	if err := clientA.SyncOnce(context.Background()); err != nil {
		clientA.Close()
		t.Fatal(err)
	}
	if len(clientA.Identity().DesiredState.Nodes) != 1 || clientA.Identity().DesiredState.Nodes[0].ID != node.NodeID || clientA.Identity().DesiredState.Nodes[0].ExitMachineID != b || clientA.Identity().DesiredState.Nodes[0].Enabled {
		clientA.Close()
		t.Fatal("A did not receive the fail-closed one-hop Node snapshot")
	}
	clientA.Close()
	clientB, err := agent.LoadClient(agentDirB, caPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientB.SyncOnce(context.Background()); err != nil || len(clientB.Identity().DesiredState.Nodes) != 0 {
		clientB.Close()
		t.Fatalf("B received another machine's Nodes: %v", err)
	}
	clientB.Close()
	repair := createJoinTokenThroughAdmin(t, admin, httpServer.URL, csrf, a)
	if id, err := agent.Pair(context.Background(), agentDirA, "", "", repair, caPath, true); err != nil || id != a {
		t.Fatalf("Node-owning machine re-pair failed: %s, %v", id, err)
	}
	clientA, err = agent.LoadClient(agentDirA, caPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Close()
	if len(clientA.Identity().DesiredState.Nodes) != 1 || clientA.Identity().DesiredState.Nodes[0].Reality.PublicKey != node.PublicKey {
		t.Fatal("re-pair changed Node identity or key")
	}
	if strings.Contains(string(body), clientA.Identity().MachineSecret) {
		t.Fatal("admin Node list leaked machine authentication")
	}
}
