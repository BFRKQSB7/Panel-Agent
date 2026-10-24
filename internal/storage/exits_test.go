package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func registerExitTestMachine(t *testing.T, store *Store, now time.Time) MachineRecord {
	t.Helper()
	ctx := context.Background()
	join, candidate, registration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func markExitMachineApplied(t *testing.T, store *Store, machine MachineRecord, now time.Time, generation uint64) {
	t.Helper()
	state, err := store.GetMachineDesiredState(context.Background(), machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	restrictionDigest, err := state.Snapshot.QuotaRestrictions.Digest()
	if err != nil {
		t.Fatal(err)
	}
	instance := mustUUID(t)
	_, err = store.RecordMachineStatus(context.Background(), machine.MachineID, protocol.StatusRequest{
		AuthorityEpoch: state.Epoch, Revision: state.Revision, Digest: state.Digest, Received: true, Applied: true,
		AppliedAuthorityEpoch: state.Epoch, AppliedRevision: state.Revision, AppliedDigest: state.Digest,
		AppliedRestrictionDigest: restrictionDigest, RestrictionAppliedAt: now.Format(time.RFC3339Nano), CoreGeneration: generation, CoreInstanceID: instance,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOneHopExitPublishesTransitOnlyAfterExitApplied(t *testing.T) {
	store := openMachineTestStore(t)
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	entry := registerExitTestMachine(t, store, now)
	exitMachine := registerExitTestMachine(t, store, now)
	exit, err := store.SaveExit(ctx, exitMachine.MachineID, ExitInput{PublicHost: "exit.example.test", PublicPort: 45100, ListenHost: "0.0.0.0", ListenPort: 19443, ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"}, now)
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.SaveNode(ctx, entry.MachineID, "", NodeInput{Name: "one-hop", PublicHost: "entry.example.test", PublicPort: 45001, ListenHost: "0.0.0.0", ListenPort: 18443, Enabled: true, ServerName: "www.cloudflare.com", ExitMachineID: exitMachine.MachineID}, now)
	if err != nil {
		t.Fatal(err)
	}
	entryState, err := store.GetMachineDesiredState(ctx, entry.MachineID)
	if err != nil || len(entryState.Snapshot.Nodes) != 1 || entryState.Snapshot.Nodes[0].Enabled || len(entryState.Snapshot.TransitOutbounds) != 0 {
		t.Fatalf("entry was enabled before exit readiness: %+v, %v", entryState.Snapshot, err)
	}
	exitState, err := store.GetMachineDesiredState(ctx, exitMachine.MachineID)
	if err != nil || exitState.Snapshot.Exit == nil || exitState.Snapshot.Exit.MachineID != exitMachine.MachineID {
		t.Fatalf("exit snapshot = %+v, %v", exitState.Snapshot, err)
	}
	if _, err := store.SaveNode(ctx, exitMachine.MachineID, "", NodeInput{Name: "self", PublicHost: "exit.example.test", PublicPort: 45101, ListenHost: "0.0.0.0", ListenPort: 19444, Enabled: true, ServerName: "www.cloudflare.com", ExitMachineID: exitMachine.MachineID}, now); !errors.Is(err, ErrInvalidExit) {
		t.Fatalf("self Exit was accepted: %v", err)
	}
	markExitMachineApplied(t, store, exitMachine, now, 1)
	entryState, err = store.GetMachineDesiredState(ctx, entry.MachineID)
	if err != nil || !entryState.Snapshot.Nodes[0].Enabled || len(entryState.Snapshot.TransitOutbounds) != 1 || entryState.Snapshot.TransitOutbounds[0].ExitMachineID != exitMachine.MachineID {
		t.Fatalf("ready Exit did not enable the original route: %+v, %v", entryState.Snapshot, err)
	}
	exitState, err = store.GetMachineDesiredState(ctx, exitMachine.MachineID)
	if err != nil || len(exitState.Snapshot.Exit.Credentials) != 1 || exitState.Snapshot.Exit.Credentials[0].EntryMachineID != entry.MachineID {
		t.Fatalf("Transit credential was not scoped to the entry machine: %+v, %v", exitState.Snapshot, err)
	}
	if entryState.Snapshot.TransitOutbounds[0].UUID != exitState.Snapshot.Exit.Credentials[0].UUID {
		t.Fatal("entry and Exit received different Transit credentials")
	}
	oldUUID := entryState.Snapshot.TransitOutbounds[0].UUID
	if err := store.RotateExitTransit(ctx, exitMachine.MachineID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	entryState, err = store.GetMachineDesiredState(ctx, entry.MachineID)
	if err != nil || entryState.Snapshot.Nodes[0].Enabled || len(entryState.Snapshot.TransitOutbounds) != 0 {
		t.Fatalf("Transit rotation did not fail closed while Exit reapplies: %+v, %v", entryState.Snapshot, err)
	}
	markExitMachineApplied(t, store, exitMachine, now.Add(2*time.Second), 2)
	entryState, err = store.GetMachineDesiredState(ctx, entry.MachineID)
	if err != nil || !entryState.Snapshot.Nodes[0].Enabled || len(entryState.Snapshot.TransitOutbounds) != 1 || entryState.Snapshot.TransitOutbounds[0].UUID == oldUUID {
		t.Fatalf("Transit rotation did not publish the new credential after Exit applied: %+v, %v", entryState.Snapshot, err)
	}
	if err := store.DeleteExit(ctx, exitMachine.MachineID, now); !errors.Is(err, ErrExitInUse) {
		t.Fatalf("Exit in use was deleted: %v", err)
	}
	if err := store.RevokeMachine(ctx, exitMachine.MachineID, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	entryState, err = store.GetMachineDesiredState(ctx, entry.MachineID)
	if err != nil || entryState.Snapshot.Nodes[0].Enabled || len(entryState.Snapshot.TransitOutbounds) != 0 {
		t.Fatalf("revoked Exit did not fail closed: %+v, %v", entryState.Snapshot, err)
	}
	_ = node
	_ = exit
}
