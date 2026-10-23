package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

// This suite is opt-in and runs against the pinned, real Linux Xray binary.
// The SSH smoke supplies it on the explicitly authorized isolated test host.
func TestRealCoreApplyAndCrashRecovery(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_XRAY")
	if binary == "" {
		t.Skip("requires explicit pinned Xray test binary")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
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
	uuid, _ := security.NewUUID()
	state := protocol.DesiredState{Nodes: []protocol.DesiredNode{{ID: nodeID, Name: "fault-test", PublicHost: "127.0.0.1", PublicPort: 19443, ListenHost: "127.0.0.1", ListenPort: 19443, Enabled: true,
		Reality:     protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"},
		Credentials: []protocol.ProxyCredential{{UserID: "development", UUID: uuid, Email: "development-" + nodeID, Development: true}}}}}
	apply := func(revision uint64, desired protocol.DesiredState) error {
		digest, _ := protocol.DigestDesiredState(desired)
		return m.Apply(ctx, epoch, revision, digest, desired)
	}
	if err := apply(1, state); err != nil {
		t.Fatal(err)
	}
	original, generation := *m.state.Process, m.state.Generation
	if err := apply(1, state); err != nil || *m.state.Process != original || m.state.Generation != generation {
		t.Fatal("unchanged snapshot restarted Core")
	}
	epoch, _ = security.NewUUID()
	if err := apply(1, state); err != nil || *m.state.Process != original || m.state.Current.AuthorityEpoch != epoch || m.state.LKG == nil {
		t.Fatal("authority switch failed to validate and retain the rollback reference")
	}
	invalid := state
	invalid.Nodes = append([]protocol.DesiredNode{}, state.Nodes...)
	invalid.Nodes[0].Reality.ShortID = "invalid"
	if err := apply(2, invalid); err == nil || *m.state.Process != original {
		t.Fatal("bad config replaced healthy Current")
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:19444")
	if err != nil {
		t.Fatal(err)
	}
	changed := state
	changed.Nodes = append([]protocol.DesiredNode{}, state.Nodes...)
	changed.Nodes[0].ListenPort = 19444
	if err := apply(2, changed); err == nil || m.state.LastError != protocol.ErrorCorePortConflict || *m.state.Process != original {
		t.Fatal("occupied port replaced Current")
	}
	_ = occupied.Close()

	// Occupy only the new port AFTER the config check and bind preflight. This
	// causes a real start/health failure while the old config can still roll back.
	blocked := make(chan net.Listener, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			contents, _ := os.ReadFile(filepath.Join(m.dir, "state.json"))
			var state coreJournal
			if json.Unmarshal(contents, &state) == nil && state.Phase == "stopping" {
				listener, _ := net.Listen("tcp", "127.0.0.1:19444")
				blocked <- listener
				return
			}
			time.Sleep(time.Millisecond)
		}
		blocked <- nil
	}()
	err = apply(2, changed)
	blocker := <-blocked
	if blocker != nil {
		defer blocker.Close()
	}
	<-watchDone
	if blocker == nil || err == nil || (m.state.LastError != protocol.ErrorCoreHealthFailed && m.state.LastError != protocol.ErrorCoreStartFailed) || m.state.Current.Revision != 1 || !m.Status().Running {
		t.Fatalf("post-preflight startup fault did not restore Current: %v (%s)", err, m.state.LastError)
	}
	if m.state.Generation <= generation {
		t.Fatal("restart did not persist a separate generation")
	}

	// Persist actual intermediate phases and restart the manager without Panel.
	// A candidate that has started but has not passed health must never be adopted.
	changed.Nodes[0].ListenPort = 19445
	rendered, err := xray.RenderConfig(changed)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(rendered.Bytes)
	digest, _ := protocol.DigestDesiredState(changed)
	crashCandidate := coreConfigRecord{AuthorityEpoch: epoch, Revision: 3, Digest: digest, ConfigHash: hex.EncodeToString(hash[:]), Mappings: rendered.Mappings, Listeners: rendered.Listeners}
	for _, phase := range []string{"validating", "stopping", "starting", "checking"} {
		t.Run(phase, func(t *testing.T) {
			next := m.state
			candidate := crashCandidate
			if err := storage.WritePrivateAtomic(m.configPath(&candidate), rendered.Bytes); err != nil {
				t.Fatal(err)
			}
			next.Candidate, next.Phase = &candidate, phase
			if err := m.save(next); err != nil {
				t.Fatal(err)
			}
			if phase != "validating" {
				boundary, err := m.captureBoundary(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := stopCoreProcess(ctx, *m.state.Process, m.binary); err != nil {
					t.Fatal(err)
				}
				if err := m.finishBoundary(boundary, false); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "checking" {
				if _, err := m.start(ctx, &candidate, "starting"); err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := NewCoreManager(dir, binary, false)
			if err != nil {
				t.Fatal(err)
			}
			m = recovered
			if err := m.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if m.state.Current.Revision != 1 || m.state.Candidate != nil || m.state.LKG == nil || !m.Status().Running {
				t.Fatal("unconfirmed candidate replaced Current/LKG")
			}
		})
	}
	boundaries, _ := os.ReadDir(filepath.Join(m.dir, "boundaries"))
	if len(boundaries) < 3 {
		t.Fatal("restart discarded old statistics mappings")
	}
	for _, entry := range boundaries {
		contents, _ := os.ReadFile(filepath.Join(m.dir, "boundaries", entry.Name()))
		var boundary coreBoundary
		if json.Unmarshal(contents, &boundary) != nil || len(boundary.Config.Mappings) != 1 || !boundary.UnobservedTail || boundary.ObservationGap == "" {
			t.Fatal("final sample boundary conceals observation uncertainty")
		}
	}
}
