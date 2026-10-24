package storage

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestPublishedUserPolicySharedAcrossMachinesAndStableAcrossRefresh(t *testing.T) {
	s := openMachineTestStore(t)
	ctx := context.Background()
	user, _ := security.NewUUID()
	node1, _ := security.NewUUID()
	node2, _ := security.NewUUID()
	machine1, _ := security.NewUUID()
	machine2, _ := security.NewUUID()
	read := func(machine, node string) []period.Policy {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		policies, err := machinePoliciesTx(ctx, tx, machine, protocol.DesiredState{Nodes: []protocol.DesiredNode{{ID: node, Credentials: []protocol.ProxyCredential{{UserID: user}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return policies
	}
	one, two, repeat := read(machine1, node1), read(machine2, node2), read(machine1, node1)
	ids := map[period.Scope]string{}
	for _, p := range one {
		ids[p.Scope] = p.ID
	}
	for _, p := range repeat {
		if ids[p.Scope] != p.ID {
			t.Fatal("refresh minted another period policy")
		}
	}
	for _, p := range two {
		if p.Scope.Kind == "user" && ids[p.Scope] != p.ID {
			t.Fatal("machine-specific global user policy")
		}
		if p.Scope.Kind != "user" {
			for _, old := range one {
				if old.ID == p.ID {
					t.Fatal("dimensions shared policy identity")
				}
			}
		}
	}
}

func TestLegacySnapshotGetsPoliciesOnOpenWithoutRotatingCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	join, candidate, registration := testSecrets(t)
	if err = s.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveNode(ctx, machine.MachineID, "", NodeInput{Name: "legacy", PublicHost: "node.example.test", PublicPort: 45001, ListenHost: "127.0.0.1", ListenPort: 18443, Enabled: true, Development: true, ServerName: "www.cloudflare.com"}, now); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	old.Snapshot.PeriodPolicies = nil
	nodesBefore, _ := json.Marshal(old.Snapshot.Nodes)
	plain, _ := json.Marshal(old.Snapshot)
	digest, _ := protocol.DigestDesiredState(old.Snapshot)
	encrypted, err := security.SealSecret(s.masterKey, plain, "machine-desired:"+machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE machines SET desired_digest=?,desired_snapshot=? WHERE machine_id=?", digest, encrypted, machine.MachineID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	nodesAfter, _ := json.Marshal(upgraded.Snapshot.Nodes)
	if len(upgraded.Snapshot.PeriodPolicies) == 0 || upgraded.Revision != old.Revision+1 || string(nodesBefore) != string(nodesAfter) {
		t.Fatal("legacy upgrade did not publish policies independently of credentials")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repeated, err := s.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil || repeated.Revision != upgraded.Revision || repeated.Digest != upgraded.Digest {
		t.Fatal("normal restart republished period policies", err)
	}
}
