package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestRealCoreExpiryWithoutPanelAndRecovery(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_XRAY")
	if binary == "" {
		t.Skip("requires explicit pinned real Xray")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		processes, _ := m.ownedProcesses()
		for _, process := range processes {
			_ = stopCoreProcess(context.Background(), process, m.binary)
		}
	})
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	epoch, _ := security.NewUUID()
	nodeID, _ := security.NewUUID()
	userID, _ := security.NewUUID()
	uuid, _ := security.NewUUID()
	desired := protocol.DesiredState{Nodes: []protocol.DesiredNode{{ID: nodeID, Name: "expiry-test", PublicHost: "127.0.0.1", PublicPort: 19447, ListenHost: "127.0.0.1", ListenPort: 19447, Enabled: true, Reality: protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{{UserID: userID, UUID: uuid, Email: "u-" + userID + "-n-" + nodeID, ExpiresAt: time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339)}}}}}
	digest, _ := protocol.DigestDesiredState(desired)
	if err := m.Apply(ctx, epoch, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	generation := m.state.Generation
	if len(m.state.Current.Mappings) != 1 || m.NextExpiry().IsZero() {
		t.Fatal("future expiry was omitted")
	}
	if until := time.Until(m.NextExpiry()); until > 0 {
		time.Sleep(until + 30*time.Millisecond)
	}
	if err := m.ReconcileExpiration(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Current.Mappings) != 0 || m.state.Generation <= generation || m.state.Current.Revision != 1 || m.state.Current.Digest != digest {
		t.Fatal("local expiry did not remove credential without inventing a Panel revision")
	}
	if err := stopCoreProcess(ctx, *m.state.Process, m.binary); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Inbounds []struct{ Settings struct{ Clients []any } }
	}
	contents, err := os.ReadFile(m.configPath(m.state.Current))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &config); err != nil || len(config.Inbounds) != 1 || len(config.Inbounds[0].Settings.Clients) != 0 {
		t.Fatal("offline restart resurrected an expired credential")
	}
	if err := os.Remove(m.desiredPath(digest)); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileExpiration(ctx); err == nil || m.Status().Running {
		t.Fatal("missing confirmed expiry source did not fail closed")
	}
}

func TestRealCoreExpiryDuringUnreachablePanel(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_XRAY")
	if binary == "" {
		t.Skip("requires explicit pinned real Xray")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	t.Cleanup(func() {
		processes, _ := m.ownedProcesses()
		for _, process := range processes {
			_ = stopCoreProcess(context.Background(), process, m.binary)
		}
	})
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	epoch, _ := security.NewUUID()
	nodeID, _ := security.NewUUID()
	userID, _ := security.NewUUID()
	uuid, _ := security.NewUUID()
	machineID, _ := security.NewUUID()
	secret, _ := security.NewToken(security.MachineSecretBytes)
	desired := protocol.DesiredState{Nodes: []protocol.DesiredNode{{ID: nodeID, Name: "unreachable-panel", PublicHost: "127.0.0.1", PublicPort: 19451, ListenHost: "127.0.0.1", ListenPort: 19451, Enabled: true, Reality: protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{{UserID: userID, UUID: uuid, Email: "u-" + userID + "-n-" + nodeID, ExpiresAt: time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339)}}}}}
	digest, _ := protocol.DigestDesiredState(desired)
	if err := m.Apply(ctx, epoch, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		close(entered)
		<-r.Context().Done()
	}))
	defer h.Close()
	caPath := filepath.Join(dir, "test-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openIdentityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(store, Identity{Version: 1, PanelURL: h.URL, MachineID: machineID, MachineSecret: secret, AuthorityEpoch: epoch, Revision: 1, Digest: digest, DesiredState: desired}, caPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.core = m
	if err := client.SyncOnce(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("Panel request did not end at the credential deadline", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("test never reached the unresponsive Panel")
	}
	if len(m.state.Current.Mappings) != 0 || !m.Status().Running {
		t.Fatal("request failure deferred local expiry until a later backoff")
	}
}
