package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestPermissionFloorPreservesOneHopAndFencesTransitRotation(t *testing.T) {
	id := func() string {
		v, e := security.NewUUID()
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	key, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	reality := protocol.RealityConfig{ServerName: "cloudflare-dns.com", Target: "1.1.1.1:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}
	entry, exit, node, user, credential, transit := id(), id(), id(), id(), id(), id()
	outbound := protocol.TransitOutbound{ExitMachineID: exit, PublicHost: "127.0.0.1", PublicPort: 18447, UUID: transit, ServerName: reality.ServerName, PublicKey: reality.PublicKey, ShortID: reality.ShortID}
	entryState := protocol.DesiredState{TrafficReportingEnabled: true, Nodes: []protocol.DesiredNode{{ID: node, Name: "onehop", PublicHost: "127.0.0.1", PublicPort: 18444, ListenHost: "127.0.0.1", ListenPort: 18444, Enabled: true, Reality: reality, ExitMachineID: exit, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: credential, Email: "u-" + user + "-n-" + node}}}}, TransitOutbounds: []protocol.TransitOutbound{outbound}}
	exitState := protocol.DesiredState{TrafficReportingEnabled: true, Nodes: []protocol.DesiredNode{}, Exit: &protocol.DesiredExit{MachineID: exit, PublicHost: "127.0.0.1", PublicPort: 18447, ListenHost: "127.0.0.1", ListenPort: 18447, Reality: reality, Credentials: []protocol.TransitCredential{{EntryMachineID: entry, UUID: transit, Email: "transit-" + entry}}}}
	for _, tc := range []struct {
		name    string
		desired protocol.DesiredState
	}{{"entry", entryState}, {"exit", exitState}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.Chmod(dir, 0700); e != nil {
				t.Fatal(e)
			}
			m := &CoreManager{dir: dir}
			floor := tc.desired
			digest, e := protocol.DigestDesiredState(floor)
			if e != nil {
				t.Fatal(e)
			}
			if e = m.persistDesired(digest, floor); e != nil {
				t.Fatal(e)
			}
			m.state.PermissionDigest = digest
			current, e := m.renderDesired(tc.desired)
			if e != nil {
				t.Fatal(e)
			}
			clamped, e := m.renderWithPermissionFloor(tc.desired, true)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(current.Bytes, clamped.Bytes) {
				t.Fatal("unchanged policy changed transport during local reconciliation")
			}
			if binary := os.Getenv("PANEL_AGENT_TEST_XRAY"); binary != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				real, e := NewCoreManager(dir, binary, false, true)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					ps, _ := real.ownedProcesses()
					for _, p := range ps {
						_ = stopCoreProcess(context.Background(), p, binary)
					}
				})
				epoch := id()
				if e = real.Apply(ctx, epoch, 1, digest, tc.desired); e != nil {
					t.Fatal(e)
				}
				before := real.Status()
				process := *real.state.Process
				for n := 0; n < 3; n++ {
					if e = real.ReconcileExpiration(ctx); e != nil {
						t.Fatal(e)
					}
				}
				after := real.Status()
				if !after.Running || after.Generation != before.Generation || *real.state.Process != process {
					t.Fatal("unchanged local reconciliation restarted or removed One-Hop transport")
				}
				next := real.state
				next.LastError = protocol.ErrorCoreConfigInvalid
				if e = real.save(next); e != nil {
					t.Fatal(e)
				}
				if e = real.ReconcileExpiration(ctx); e != nil {
					t.Fatal(e)
				}
				if real.Status().Error == "" {
					t.Fatal("offline local reconciliation erased an unresolved apply error")
				}
				if e = real.Apply(ctx, epoch, 1, digest, tc.desired); e != nil {
					t.Fatal(e)
				}
				if real.Status().Error != "" || real.Status().Generation != before.Generation || *real.state.Process != process {
					t.Fatal("authenticated unchanged snapshot did not clear the old apply failure without restarting")
				}
			}
			// A rollback to the old transport must not recover the previous secret.
			if tc.name == "entry" {
				floor.TransitOutbounds = append([]protocol.TransitOutbound(nil), floor.TransitOutbounds...)
				floor.TransitOutbounds[0].UUID = id()
			} else {
				copyExit := *floor.Exit
				copyExit.Credentials = append([]protocol.TransitCredential(nil), copyExit.Credentials...)
				copyExit.Credentials[0].UUID = id()
				floor.Exit = &copyExit
			}
			digest, e = protocol.DigestDesiredState(floor)
			if e != nil {
				t.Fatal(e)
			}
			if e = m.persistDesired(digest, floor); e != nil {
				t.Fatal(e)
			}
			m.state.PermissionDigest = digest
			clamped, e = m.renderWithPermissionFloor(tc.desired, true)
			if e != nil {
				t.Fatal(e)
			}
			if bytes.Contains(clamped.Bytes, []byte(transit)) || len(clamped.Mappings) != 0 {
				t.Fatal("old transit credential was restored by permission-floor fallback")
			}
		})
	}
}
