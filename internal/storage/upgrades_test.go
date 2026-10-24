package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestUpgradeIntentPullResultIsIdempotentAndAuthorityBound(t *testing.T) {
	store := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	join, candidate, registration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, authority, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.CreateUpgradeIntent(ctx, UpgradeIntentInput{MachineID: machine.MachineID, TargetVersion: "26.10.22", Source: protocol.UpgradeSourceOfficialXray, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PackageSize: 1024}, now)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != protocol.UpgradeStatePending || intent.AuthorityEpoch != authority.Epoch {
		t.Fatalf("created intent = %+v", intent)
	}
	intents, err := store.PullUpgradeIntents(ctx, machine.MachineID, authority.Epoch, now)
	if err != nil || len(intents) != 1 || intents[0].State != protocol.UpgradeStateRunning {
		t.Fatalf("pulled intents = (%+v, %v)", intents, err)
	}
	repeated, err := store.PullUpgradeIntents(ctx, machine.MachineID, authority.Epoch, now)
	if err != nil || len(repeated) != 1 || repeated[0].ID != intent.ID {
		t.Fatalf("repeated pull changed intent = (%+v, %v)", repeated, err)
	}
	result := protocol.UpgradeResultRequest{Version: protocol.UpgradeProtocolVersion, RequestID: "11111111-1111-4111-8111-111111111111", IntentID: intent.ID, AuthorityEpoch: authority.Epoch, State: protocol.UpgradeStateFailed, TargetVersion: intent.TargetVersion, ResultCode: protocol.UpgradeErrorChecksum, ResultDetail: "checksum mismatch", RolledBack: false}
	if err := store.RecordUpgradeResult(ctx, machine.MachineID, result, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUpgradeResult(ctx, machine.MachineID, result, now.Add(time.Second)); err != nil {
		t.Fatalf("duplicate result was not idempotent: %v", err)
	}
	result.ResultCode = protocol.UpgradeErrorHealth
	if err := store.RecordUpgradeResult(ctx, machine.MachineID, result, now); !errors.Is(err, ErrUpgradeIntentTerminal) {
		t.Fatalf("conflicting terminal result = %v", err)
	}
	if _, err := store.RotatePanelAuthority(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PullUpgradeIntents(ctx, machine.MachineID, authority.Epoch, now); !errors.Is(err, ErrUpgradeIntentStale) {
		t.Fatalf("stale authority pull = %v", err)
	}
}
