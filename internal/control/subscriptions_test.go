package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestPrivateLinksAuthorizationRetryAndSubscriptionProjection(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashAdminPassword([]byte("d06-test-admin-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dir, hash); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewTLSServer(s.Handler())
	defer h.Close()
	admin, base := newTestClient(t, h.URL)
	admin.Transport = h.Client().Transport
	loginControlAdmin(t, admin, base, "d06-test-admin-password")
	csrf := csrfCookie(t, admin, base)
	public := h.Client()
	public.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var logs bytes.Buffer
	originalLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(originalLog)
	call := func(client *http.Client, method, path, body, origin string, code int, fetchSite ...string) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if len(fetchSite) != 0 {
			req.Header.Set("Sec-Fetch-Site", fetchSite[0])
		}
		if client == admin {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != code {
			t.Fatalf("request %s expected %d, got %d: %s (%v)", method, code, res.StatusCode, data, err)
		}
		if !strings.Contains(res.Header.Get("Cache-Control"), "no-store") || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("private response lacks privacy headers")
		}
		return data, res.Header
	}
	join, _ := security.NewToken(32)
	candidate, _ := security.NewToken(32)
	registration, _ := security.NewUUID()
	if err := s.store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.store.RegisterMachine(ctx, storage.RegisterMachineInput{RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate), Hostname: "private-host", OS: "linux", Architecture: "amd64", CPUCount: 1, MemoryBytes: 128 << 20, AgentVersion: protocol.AgentVersion}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nodes := []storage.NodeRecord{}
	for i := 0; i < 3; i++ {
		node, err := s.store.SaveNode(ctx, machine.MachineID, "", storage.NodeInput{Name: "同名 / 节点 #", PublicHost: "2001:db8::1", PublicPort: 45001 + i, ListenHost: "127.0.0.1", ListenPort: 18443 + i, Enabled: true, ServerName: "www.cloudflare.com"}, now)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	group, err := s.store.SaveGroup(ctx, "node", "", storage.GroupInput{Name: "first", NodeIDs: []string{nodes[0].NodeID, nodes[1].NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	overlap, err := s.store.SaveGroup(ctx, "node", "", storage.GroupInput{Name: "overlap", NodeIDs: []string{nodes[1].NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	ug, err := s.store.SaveGroup(ctx, "user", "", storage.GroupInput{Name: "ordinary", NodeGroupIDs: []string{group.GroupID, overlap.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.store.SaveUser(ctx, "", storage.UserInput{Name: "Alice", UserGroupID: ug.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.SaveUser(ctx, "", storage.UserInput{Name: "Bob", UserGroupID: ug.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	issue := func() string {
		t.Helper()
		data, _ := call(admin, "GET", "/api/admin/user-tokens/action", "", "", 200)
		var result map[string]string
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result["action_token"]
	}
	action := issue()
	batch := tokenBatchRequest{Kind: "both", ActionToken: action, Items: []storage.TokenMutation{{UserID: user.UserID}, {UserID: "missing-user"}}}
	encoded, _ := json.Marshal(batch)
	call(public, "POST", "/api/admin/user-tokens/batch", string(encoded), base, 401)
	call(admin, "POST", "/api/admin/user-tokens/batch", string(encoded), "https://attacker.invalid", 403)
	data, _ := call(admin, "POST", "/api/admin/user-tokens/batch", string(encoded), base, 200)
	var result struct {
		Results []storage.TokenDelivery `json:"results"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 4 || result.Results[1].Error != "user_not_found" || result.Results[3].Error != "user_not_found" {
		t.Fatal("batch partial failure missing")
	}
	dash, sub := result.Results[0].Token, result.Results[2].Token
	data, _ = call(admin, "POST", "/api/admin/user-tokens/batch", string(encoded), base, 200)
	result.Results = nil
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Results[0].AlreadyApplied || !result.Results[2].AlreadyApplied || result.Results[0].Token != "" || result.Results[2].Token != "" {
		t.Fatal("retry redisclosed or rotated successful tokens")
	}
	call(public, "GET", "/u/"+sub, "", "", 404)
	call(public, "GET", "/s/"+dash+"/uri", "", "", 404)
	dashboard, _ := call(public, "GET", "/u/"+dash, "", "", 200)
	if !bytes.Contains(dashboard, []byte("本周期全局实际用量")) || !bytes.Contains(dashboard, []byte("心跳失联")) || bytes.Contains(dashboard, []byte(sub)) {
		t.Fatal("dashboard leaks subscription token or invents usage/online status")
	}
	match := regexp.MustCompile(`name="action_token" value="([^"]+)"`).FindSubmatch(dashboard)
	if len(match) != 2 {
		t.Fatal("missing signed user action")
	}
	userAction := string(match[1])
	resetBody, _ := json.Marshal(map[string]any{"action_token": userAction, "spec": map[string]string{}})
	call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "https://attacker.invalid", 403)
	call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "", 403)
	call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "null", 403)
	call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "null", 403, "cross-site")
	call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "null", 403, "same-site")
	call(public, "POST", "/u/"+dash+"/subscription/reset", `{}`, "null", 403, "same-origin")
	call(public, "POST", "/u/"+dash+"/subscription/reset", `{"action_token":"`+userAction+`","quota_bytes":0}`, base, 400)
	before, _ := s.store.GetUserTokenState(ctx, user.UserID)
	call(public, "GET", "/u/"+dash+"/subscription/reset", "", "", 303)
	after, _ := s.store.GetUserTokenState(ctx, user.UserID)
	if before != after {
		t.Fatal("GET mutated token")
	}
	uri, headers := call(public, "GET", "/s/"+sub+"/uri", "", "", 200)
	if headers.Get("Subscription-Userinfo") != "" {
		t.Fatal("invented usage header")
	}
	lines := strings.Fields(string(uri))
	if len(lines) != 2 {
		t.Fatal("authorization was not deduplicated")
	}
	for _, line := range lines {
		u, err := url.Parse(line)
		if err != nil || u.User.Username() != user.CredentialUUID || u.Hostname() != "2001:db8::1" || (u.Port() != "45001" && u.Port() != "45002") || u.Fragment != nodes[0].Name || u.Query().Get("pbk") == "" {
			t.Fatal("URI omitted legitimate credentials or public endpoint", err)
		}
	}
	mihomo, _ := call(public, "GET", "/s/"+sub+"/mihomo", "", "", 200)
	var profile struct {
		Proxies []struct {
			Name, UUID string
			Reality    map[string]any `json:"reality-opts"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(mihomo, &profile); err != nil {
		t.Fatal(err)
	}
	if len(profile.Proxies) != 2 || profile.Proxies[0].Name == profile.Proxies[1].Name || profile.Proxies[0].UUID != user.CredentialUUID || profile.Proxies[0].Reality["support-x25519mlkem768"] != true {
		t.Fatal("invalid Mihomo credential projection")
	}
	state, err := s.store.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{state.Snapshot.Nodes[0].Reality.PrivateKey, candidate, machine.TokenHash, machine.MachineID, other.CredentialUUID, nodes[2].PublicKey} {
		if secret == "" {
			t.Fatal("empty secret fixture")
		}
		for _, body := range [][]byte{dashboard, uri, mihomo} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatal("management or other-user secret leaked")
			}
		}
	}
	call(public, "GET", "/s/"+sub+"/sing-box", "", "", 422)
	data, _ = call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), "null", 200, "same-origin")
	var delivery storage.TokenDelivery
	if err := json.Unmarshal(data, &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.Token == "" || delivery.Token == sub {
		t.Fatal("subscription did not rotate")
	}
	data, _ = call(public, "POST", "/u/"+dash+"/subscription/reset", string(resetBody), base, 200)
	var retry storage.TokenDelivery
	if err := json.Unmarshal(data, &retry); err != nil {
		t.Fatal(err)
	}
	if !retry.AlreadyApplied || retry.Token != "" {
		t.Fatal("public retry was not idempotent")
	}
	call(public, "GET", "/s/"+sub+"/uri", "", "", 404)
	call(public, "GET", "/u/"+dash, "", "", 200)
	afterUser, _ := s.store.GetUser(ctx, user.UserID)
	if afterUser.CredentialUUID != user.CredentialUUID {
		t.Fatal("URL reset changed VLESS UUID")
	}
	call(public, "GET", "//u/"+dash+"?secret="+sub, "", "", 307)
	if strings.Contains(logs.String(), dash) || strings.Contains(logs.String(), sub) || strings.Contains(logs.String(), delivery.Token) {
		t.Fatal("private URL reached access log")
	}
	// Admin rotation invalidates the dashboard action scope without touching the
	// independent subscription family; authority rotation also invalidates actions.
	op, _ := security.NewUUID()
	newDash, err := s.store.ResetUserToken(ctx, op, "dashboard", storage.TokenMutation{UserID: user.UserID}, now)
	if err != nil {
		t.Fatal(err)
	}
	call(public, "GET", "/u/"+dash, "", "", 404)
	call(public, "POST", "/u/"+newDash.Token+"/subscription/reset", string(resetBody), base, 403)
	call(public, "GET", "/s/"+delivery.Token+"/uri", "", "", 200)
	if _, err := s.store.RotatePanelAuthority(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	call(admin, "POST", "/api/admin/user-tokens/batch", string(encoded), base, 403)
	afterUser.Enabled = false
	if _, err := s.store.SaveUser(ctx, user.UserID, afterUser.UserInput, time.Now()); err != nil {
		t.Fatal(err)
	}
	empty, _ := call(public, "GET", "/s/"+delivery.Token+"/uri", "", "", 200)
	if len(empty) != 0 {
		t.Fatal("disabled user exported active nodes")
	}
	if err := s.store.DeleteUser(ctx, user.UserID, time.Now()); err != nil {
		t.Fatal(err)
	}
	call(public, "GET", "/u/"+newDash.Token, "", "", 404)
	call(public, "GET", "/s/"+delivery.Token+"/uri", "", "", 404)
}
