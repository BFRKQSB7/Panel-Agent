package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestQuotaAdminFormsAPIsAndUsagePages(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := "d10-quota-control-password"
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
	h := httptest.NewTLSServer(server.Handler())
	defer h.Close()
	admin, base := newTestClient(t, h.URL)
	admin.Transport = h.Client().Transport
	loginControlAdmin(t, admin, base, password)
	csrf := fetchMachinesCSRF(t, admin, base)
	ctx := context.Background()
	now := time.Now().UTC()
	join, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := server.store.RegisterMachine(ctx, storage.RegisterMachineInput{RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate), Hostname: "quota-machine", DisplayName: "Quota Machine", OS: "linux", Architecture: "amd64", CPUCount: 2, MemoryBytes: 1 << 30, AgentVersion: protocol.AgentVersion}, now)
	if err != nil {
		t.Fatal(err)
	}
	node, err := server.store.SaveNode(ctx, machine.MachineID, "", storage.NodeInput{Name: "quota-node", PublicHost: "node.example.test", PublicPort: 45001, ListenHost: "127.0.0.1", ListenPort: 18443, Enabled: true, ServerName: "www.cloudflare.com"}, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeGroup, err := server.store.SaveGroup(ctx, "node", "", storage.GroupInput{Name: "quota-nodes", NodeIDs: []string{node.NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	userGroup, err := server.store.SaveGroup(ctx, "user", "", storage.GroupInput{Name: "quota-users", NodeGroupIDs: []string{nodeGroup.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := server.store.SaveUser(ctx, "", storage.UserInput{Name: "quota-user", UserGroupID: userGroup.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	api := func(method, path, body string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", base)
		request.Header.Set("X-CSRF-Token", csrf)
		response, err := admin.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		contents, _ := io.ReadAll(response.Body)
		return response.StatusCode, contents
	}
	if status, _ := api(http.MethodPut, "/api/admin/machines/"+machine.MachineID+"/quota", `{"budget_bytes":"100","warning_bytes":"50","hard_stop":true}`); status != http.StatusOK {
		t.Fatalf("machine quota API=%d", status)
	}
	if status, body := api(http.MethodGet, "/api/admin/machines/"+machine.MachineID+"/quota", ""); status != http.StatusOK || !strings.Contains(string(body), `"budget_bytes":"100"`) {
		t.Fatalf("machine quota GET=%d body=%s", status, body)
	}
	if status, _ := api(http.MethodPut, "/api/admin/users/"+user.UserID+"/nodes/"+node.NodeID+"/quota", `{"quota_bytes":"0"}`); status != http.StatusOK {
		t.Fatalf("user-node quota API=%d", status)
	}
	form := url.Values{"csrf": {csrf}, "quota_mode": {"limit"}, "quota_bytes": {"1"}}
	response := postForm(t, admin, base+"/admin/users/"+user.UserID+"/nodes/"+node.NodeID+"/quota", form, base)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("user-node quota form=%d", response.StatusCode)
	}
	if status, body := api(http.MethodGet, "/api/admin/users/"+user.UserID+"/quota", ""); status != http.StatusOK || !strings.Contains(string(body), "quota") {
		t.Fatalf("user quota GET=%d body=%s", status, body)
	}
	scope, _ := json.Marshal(period.Scope{Kind: "user", UserID: user.UserID})
	if status, _ := api(http.MethodPost, "/api/admin/quotas/reset", string(scope)); status != http.StatusOK {
		t.Fatalf("quota reset API=%d", status)
	}
	for _, path := range []string{"/admin/users", "/admin/machines", "/admin/machines/" + machine.MachineID + "/nodes"} {
		response, err := admin.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		contents, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(contents), "额度") && !strings.Contains(string(contents), "预算") {
			t.Fatalf("quota page %s status=%d body=%s", path, response.StatusCode, contents)
		}
	}
}
