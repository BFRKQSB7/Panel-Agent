package control

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent"
	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

// All children have a parent-death signal and the test has a hard timeout at
// invocation. No system service, public listener or firewall rule is created.
func TestOfficialHysteriaPanelClientSubscriptionAndRevocation(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_HYSTERIA")
	if binary == "" {
		t.Skip("PANEL_AGENT_TEST_HYSTERIA not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dir, "test-hy2-admin-hash"); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var panelOffline atomic.Bool
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if panelOffline.Load() {
			http.Error(w, "temporary control outage", http.StatusServiceUnavailable)
			return
		}
		server.Handler().ServeHTTP(w, r)
	}))
	defer h.Close()
	pair := h.TLS.Certificates[0]
	privateKey, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}))
	sni := h.Certificate().DNSNames[0]
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, []byte(cert), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	join, _ := security.NewToken(32)
	if err := server.store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "agent")
	machine, err := agent.Pair(ctx, agentDir, h.URL, "hy2-end-to-end", join, caPath, false)
	if err != nil {
		t.Fatal(err)
	}
	pkg := testExtensionPackage()
	pkg.Manifest.ID = "official-hysteria2"
	pkg.Manifest.Core = extensions.CoreSupport{Kind: "hysteria", MinVersion: "2.13.0"}
	pkg.Renderers.Xray = extensions.XrayRenderer{}
	pkg.Renderers.Hysteria = &extensions.HysteriaRenderer{Kind: "hysteria2_tls", Version: 2}
	pkg.Renderers.URI.Kind = "hysteria2_tls"
	pkg.Renderers.Mihomo.Kind = "hysteria2_tls"
	if _, err := server.store.InstallExtension(ctx, pkg, now); err != nil {
		t.Fatal(err)
	}
	if err := server.store.SetExtensionEnabled(ctx, pkg.Manifest.ID, pkg.Manifest.Version, true, now); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	udp.Close()
	node, err := server.store.SaveNodeWithExtension(ctx, machine, "", storage.NodeInput{Name: "hy2-local", PublicHost: "127.0.0.1", PublicPort: port, ListenHost: "127.0.0.1", ListenPort: port, Enabled: true, ServerName: sni}, &storage.NodeExtensionInput{ID: pkg.Manifest.ID, Version: pkg.Manifest.Version, Values: map[string]string{"sni": sni, "certificate_pem": cert, "private_key_pem": key}}, now)
	if err != nil {
		t.Fatal(err)
	}
	ng, err := server.store.SaveGroup(ctx, "node", "", storage.GroupInput{Name: "hy2-nodes", NodeIDs: []string{node.NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	ug, err := server.store.SaveGroup(ctx, "user", "", storage.GroupInput{Name: "hy2-users", NodeGroupIDs: []string{ng.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := server.store.SaveUser(ctx, "", storage.UserInput{Name: "hy2-user", UserGroupID: ug.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := security.NewUUID()
	delivery, err := server.store.ResetUserToken(ctx, operation, "subscription", storage.TokenMutation{UserID: user.UserID}, now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := agent.LoadClient(agentDir, caPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.EnableCores(ctx, os.Getenv("PANEL_AGENT_TEST_XRAY"), binary, false, true); err != nil {
		t.Fatal(err)
	}
	if err := client.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := server.store.GetMachine(ctx, machine)
	if err != nil {
		t.Fatal(err)
	}
	if status.AppliedRevision != status.DesiredRevision || status.ApplyError != "" || status.AppliedRestrictionDigest == "" || status.RestrictionAppliedAt == "" {
		t.Fatalf("invalid applied status: %+v", status)
	}
	response, err := h.Client().Get(h.URL + "/s/" + delivery.Token + "/uri")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("subscription: %s %v", data, err)
	}
	uri, err := url.Parse(strings.TrimSpace(string(data)))
	if err != nil || uri.Scheme != "hysteria2" || uri.User.Username() != user.CredentialUUID || uri.Query().Get("insecure") != "0" {
		t.Fatalf("invalid URI %s: %v", data, err)
	}
	socks, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	socksAddr := socks.Addr().String()
	socks.Close()
	config := map[string]any{"server": uri.Host, "auth": uri.User.Username(), "tls": map[string]any{"sni": uri.Query().Get("sni"), "ca": caPath}, "socks5": map[string]any{"listen": socksAddr}}
	udpForward, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpForwardAddr := udpForward.LocalAddr().String()
	udpForward.Close()
	config["udpForwarding"] = []map[string]any{{"listen": udpForwardAddr, "remote": "1.1.1.1:53", "timeout": "3s"}}
	encoded, _ := json.Marshal(config)
	configPath := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "client", "--config", configPath)
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", socksAddr, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("official client did not authenticate from subscription")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("Panel desired state, Agent hy2-only applied status and subscription authentication passed")
	if os.Getenv("PANEL_AGENT_TEST_HYSTERIA_FORWARD") == "1" {
		proxyURL, _ := url.Parse("socks5://" + socksAddr)
		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		business := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		// A literal public address avoids the development host's fake-IP DNS
		// range, which is not routable from the isolated WSL network.
		resp, err := business.Get("http://1.1.1.1/cdn-cgi/trace")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("business forwarding failed: %d %v", resp.StatusCode, readErr)
		}
		if err := client.SyncOnce(ctx); err != nil {
			t.Fatal(err)
		}
		usage, err := server.store.GetUserQuotaSummary(ctx, user.UserID, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if usage.Global.UsedBytes == 0 {
			t.Fatal("real forwarded traffic not ingested by Panel")
		}
		t.Logf("real HTTP forwarding and Panel ingress passed: %d bytes", usage.Global.UsedBytes)
		panelOffline.Store(true)
		if err := client.SyncOnce(ctx); err == nil {
			t.Fatal("control outage not detected")
		}
		udpConn, err := net.Dial("udp4", udpForwardAddr)
		if err != nil {
			t.Fatal(err)
		}
		if err := udpConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			udpConn.Close()
			t.Fatal(err)
		}
		query := []byte{0x42, 0x61, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
		if _, err := udpConn.Write(query); err != nil {
			udpConn.Close()
			t.Fatal(err)
		}
		answer := make([]byte, 2048)
		n, err := udpConn.Read(answer)
		udpConn.Close()
		if err != nil || n < 12 || answer[0] != query[0] || answer[1] != query[1] || answer[2]&0x80 == 0 {
			t.Fatalf("UDP DNS forwarding failed: n=%d %v", n, err)
		}
		panelOffline.Store(false)
		if err := client.SyncOnce(ctx); err != nil {
			t.Fatal(err)
		}
		after, err := server.store.GetUserQuotaSummary(ctx, user.UserID, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if after.Global.UsedBytes <= usage.Global.UsedBytes {
			t.Fatal("offline UDP traffic not recovered into Panel")
		}
		t.Logf("UDP forwarding survived control outage and reconciled: %d bytes", after.Global.UsedBytes)
	}
	if _, err := server.store.SaveUser(ctx, user.UserID, storage.UserInput{Name: user.Name, UserGroupID: ug.GroupID, Enabled: false}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := client.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	probe, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal("revoked hy2 instance still owns UDP listener")
	}
	probe.Close()
	t.Log("Panel user revocation stopped existing official instance")
}
