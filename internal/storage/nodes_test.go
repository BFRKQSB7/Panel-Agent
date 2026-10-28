package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestLegacyProductionSnapshotEnablesReportingWithNewRevision(t *testing.T) {
	store := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	join, candidate, registration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveNode(ctx, machine.MachineID, "", NodeInput{Name: "legacy", PublicHost: "edge.example.test", PublicPort: 45001, ListenHost: "127.0.0.1", ListenPort: 18443, ServerName: "example.com", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	authority, err := store.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if !authority.Snapshot.TrafficReportingEnabled {
		t.Fatal("new production snapshot omitted reporting")
	}
	authority.Snapshot.TrafficReportingEnabled = false
	legacyDigest, err := protocol.DigestDesiredState(authority.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(authority.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := security.SealSecret(store.masterKey, plain, "machine-desired:"+machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE machines SET desired_digest=?,desired_snapshot=? WHERE machine_id=?", legacyDigest, sealed, machine.MachineID); err != nil {
		t.Fatal(err)
	}
	requestID, _ := security.NewUUID()
	response, err := store.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{RequestID: requestID, AuthorityEpoch: authority.Epoch, Revision: authority.Revision, Digest: legacyDigest})
	if err != nil {
		t.Fatal(err)
	}
	if response.DesiredState == nil || !response.DesiredState.TrafficReportingEnabled || response.Revision != authority.Revision+1 || response.Digest == legacyDigest {
		t.Fatal("legacy snapshot changed without a new authenticated revision")
	}
	again, err := store.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{RequestID: requestID, AuthorityEpoch: response.AuthorityEpoch, Revision: response.Revision, Digest: response.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Unchanged || again.Revision != response.Revision {
		t.Fatal("reporting migration repeated on every pull")
	}
}

func TestNodeSnapshotsArePrivateScopedAndKeepTombstones(t *testing.T) {
	store := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	register := func() (MachineRecord, PanelAuthority) {
		t.Helper()
		join, candidate, registration := testSecrets(t)
		if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		machine, authority, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
		if err != nil {
			t.Fatal(err)
		}
		return machine, authority
	}
	a, initial := register()
	b, _ := register()
	input := NodeInput{Name: "测试入口", PublicHost: "node.example.test", PublicPort: 45001, ListenHost: "127.0.0.1", ListenPort: 18443, ServerName: "www.cloudflare.com", Enabled: true, Development: true}
	one, err := store.SaveNode(ctx, a.MachineID, "", input, now)
	if err != nil {
		t.Fatal(err)
	}
	input.ListenPort++
	two, err := store.SaveNode(ctx, a.MachineID, "", input, now)
	if err != nil {
		t.Fatal(err)
	}
	input.ListenPort++
	input.Development = false
	three, err := store.SaveNode(ctx, a.MachineID, "", input, now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.GetMachineDesiredState(ctx, a.MachineID)
	if err != nil || len(state.Snapshot.Nodes) != 3 || state.Revision != 4 {
		t.Fatalf("machine A snapshot = %+v, %v", state, err)
	}
	var developmentCredentials []protocol.ProxyCredential
	for _, node := range state.Snapshot.Nodes {
		if node.PublicPort != 45001 || node.ListenPort == node.PublicPort {
			t.Fatal("explicit public/listen port mapping was changed")
		}
		developmentCredentials = append(developmentCredentials, node.Credentials...)
		if node.ID == three.NodeID && len(node.Credentials) != 0 {
			t.Fatal("normal Node unexpectedly contains a development credential")
		}
		var encryptedKey []byte
		if err := store.db.QueryRowContext(ctx, "SELECT private_key FROM nodes WHERE node_id = ?", node.ID).Scan(&encryptedKey); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encryptedKey, []byte(node.Reality.PrivateKey)) {
			t.Fatal("Node private key was stored in plaintext")
		}
	}
	if len(developmentCredentials) != 2 || developmentCredentials[0].UUID != developmentCredentials[1].UUID || developmentCredentials[0].Email == developmentCredentials[1].Email {
		t.Fatal("development credentials lost shared-UUID/separate-email accounting")
	}
	var encryptedSnapshot []byte
	if err := store.db.QueryRowContext(ctx, "SELECT desired_snapshot FROM machines WHERE machine_id = ?", a.MachineID).Scan(&encryptedSnapshot); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encryptedSnapshot, []byte(developmentCredentials[0].UUID)) {
		t.Fatal("snapshot credentials were stored in plaintext")
	}
	private := state.Snapshot.Nodes[0].Reality.PrivateKey
	encoded, _ := json.Marshal(one)
	if bytes.Contains(encoded, []byte(private)) || bytes.Contains(encoded, []byte("private_key")) {
		t.Fatal("Node admin response exposes its private key")
	}
	request := protocol.DesiredStateRequest{AuthorityEpoch: initial.Epoch, Revision: initial.Revision, Digest: initial.Digest}
	response, err := store.DesiredStateExchange(ctx, b.MachineID, request)
	if err != nil || !response.Unchanged || response.DesiredState != nil || response.Revision != 1 {
		t.Fatalf("machine B received A's Node state: %+v, %v", response, err)
	}
	response, err = store.DesiredStateExchange(ctx, a.MachineID, request)
	if err != nil || response.DesiredState == nil || len(response.DesiredState.Nodes) != 3 {
		t.Fatalf("machine A did not receive its own snapshot: %+v, %v", response, err)
	}
	input.ListenPort = two.ListenPort
	if _, err := store.SaveNode(ctx, a.MachineID, one.NodeID, input, now); !errors.Is(err, ErrNodePortConflict) {
		t.Fatalf("duplicate controlled port accepted: %v", err)
	}
	if _, err := store.SaveNode(ctx, b.MachineID, one.NodeID, input, now); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("cross-machine Node edit accepted: %v", err)
	}
	if err := store.DeleteNode(ctx, a.MachineID, one.NodeID, now); err != nil {
		t.Fatal(err)
	}
	state, err = store.GetMachineDesiredState(ctx, a.MachineID)
	if err != nil || len(state.Snapshot.Nodes) != 2 || state.Revision != 5 {
		t.Fatalf("Node deletion did not publish a new snapshot: %+v, %v", state, err)
	}
	var tombstones int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE node_id = ? AND deleted_at IS NOT NULL", one.NodeID).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("deleted Node identity was discarded: %d, %v", tombstones, err)
	}
}
