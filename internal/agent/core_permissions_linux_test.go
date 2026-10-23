package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestRealCoreFallbackDoesNotUndoRevocation(t *testing.T) {
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
	userID, _ := security.NewUUID()
	keepID, _ := security.NewUUID()
	oldUUID, _ := security.NewUUID()
	keepUUID, _ := security.NewUUID()
	desired := protocol.EmptyDesiredState()
	for _, port := range []int{19448, 19450} {
		id, _ := security.NewUUID()
		desired.Nodes = append(desired.Nodes, protocol.DesiredNode{ID: id, Name: "permission-test", PublicHost: "127.0.0.1", PublicPort: port, ListenHost: "127.0.0.1", ListenPort: port, Enabled: true, Reality: protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{{UserID: userID, UUID: oldUUID, Email: "u-" + userID + "-n-" + id}, {UserID: keepID, UUID: keepUUID, Email: "u-" + keepID + "-n-" + id}}})
	}
	digest, _ := protocol.DigestDesiredState(desired)
	if err := m.Apply(ctx, epoch, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	desired.Nodes[0].ListenPort = 19449
	desired.Nodes[1].Enabled = false
	newUUID, _ := security.NewUUID()
	desired.Nodes[0].Credentials[0].UUID = newUUID
	digest, _ = protocol.DigestDesiredState(desired)
	if err := m.Apply(ctx, epoch, 2, digest, desired); err != nil {
		t.Fatal(err)
	}
	if err := stopCoreProcess(ctx, *m.state.Process, m.binary); err != nil {
		t.Fatal(err)
	}
	blocker, err := net.Listen("tcp", "127.0.0.1:19449")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if m.state.Current.Revision != 1 || m.state.PermissionDigest != digest || len(m.state.Current.Mappings) != 1 || m.state.Current.Mappings[0].UserID != keepID || m.state.Current.Mappings[0].NodeID != desired.Nodes[0].ID {
		t.Fatal("older LKG restored a revoked Node or old rotated credential")
	}
	if err := m.ReconcileExpiration(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Current.Mappings) != 1 || m.state.PermissionDigest != digest {
		t.Fatal("local reconciliation weakened the confirmed permission policy")
	}
	_ = blocker.Close()
	if err := m.Apply(ctx, epoch, 2, digest, desired); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Current.Mappings) != 2 || m.state.Current.Revision != 2 {
		t.Fatal("valid Current could not recover after the transient port conflict")
	}
}
