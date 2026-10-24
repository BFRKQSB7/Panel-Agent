package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestTrafficRecoveryGapUsesReceiptAndAddsNoUsage(t *testing.T) {
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
	epoch, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.InstallationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO traffic_ledgers(machine_id,ledger_epoch,installation_id,allocated_close_seq,closed_through,reconciliation_pending,updated_at) VALUES(?,?,?,?,?,?,?)", machine.MachineID, epoch, installation, 5, 5, 1, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	receipt, err := security.SignClosedReceipt(store.masterKey, security.ClosedClaims{Version: 1, InstallationID: installation, MachineID: machine.MachineID, LedgerEpoch: epoch, ClosedThrough: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTrafficRecoveryGap(ctx, TrafficRecoveryGapInput{MachineID: machine.MachineID, LedgerEpoch: epoch, AgentAllocated: 10, FirstSeq: 6, LastSeq: 10, Receipt: receipt}, now); err != nil {
		t.Fatal(err)
	}
	var allocated, closed, pending int64
	if err := store.db.QueryRowContext(ctx, "SELECT allocated_close_seq,closed_through,reconciliation_pending FROM traffic_ledgers WHERE machine_id=?", machine.MachineID).Scan(&allocated, &closed, &pending); err != nil {
		t.Fatal(err)
	}
	if allocated != 10 || closed != 10 || pending != 0 {
		t.Fatalf("reconciled ledger = allocated %d closed %d pending %d", allocated, closed, pending)
	}
	var usage int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM traffic_usage WHERE machine_id=?", machine.MachineID).Scan(&usage); err != nil || usage != 0 {
		t.Fatalf("recovery gap fabricated usage: count=%d err=%v", usage, err)
	}
	gaps, err := store.ListTrafficRecoveryGaps(ctx, machine.MachineID)
	if err != nil || len(gaps) != 1 || gaps[0].FirstSeq != 6 || gaps[0].LastSeq != 10 {
		t.Fatalf("recovery gaps = (%+v, %v)", gaps, err)
	}
	response, err := store.RecoverTraffic(ctx, machine.MachineID, protocol.TrafficRecoveryRequest{Version: protocol.TrafficVersion, RequestID: "33333333-3333-4333-8333-333333333333", AuthorityEpoch: authority.Epoch, MachineID: machine.MachineID, LedgerEpoch: epoch, InstallationID: installation, Allocated: 10, Receipt: &receipt}, now)
	if err != nil || !response.Resume {
		t.Fatalf("Agent recovery did not resume after explicit gap: (%+v, %v)", response, err)
	}
}

func TestTrafficRecoveryGapRejectsHeldOrPanelDetails(t *testing.T) {
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
	machine, _, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.InstallationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO traffic_ledgers(machine_id,ledger_epoch,installation_id,allocated_close_seq,closed_through,reconciliation_pending,updated_at) VALUES(?,?,?,?,?,?,?)", machine.MachineID, epoch, installation, 5, 5, 1, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	receipt, err := security.SignClosedReceipt(store.masterKey, security.ClosedClaims{Version: 1, InstallationID: installation, MachineID: machine.MachineID, LedgerEpoch: epoch, ClosedThrough: 10})
	if err != nil {
		t.Fatal(err)
	}
	heldErr := store.RecordTrafficRecoveryGap(ctx, TrafficRecoveryGapInput{MachineID: machine.MachineID, LedgerEpoch: epoch, AgentAllocated: 10, FirstSeq: 6, LastSeq: 10, Held: []protocol.CloseRange{{First: 8, Last: 10}}, Receipt: receipt}, now)
	if !errors.Is(heldErr, ErrTrafficGapHeld) {
		t.Fatalf("held recovery range was accepted: %v", heldErr)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO traffic_segments(machine_id,ledger_epoch,stream_id,segment_id,body,accepted_up,accepted_down,finalized,close_seq,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, machine.MachineID, epoch, "stream", "segment", "{}", 0, 0, 1, 6, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	detailErr := store.RecordTrafficRecoveryGap(ctx, TrafficRecoveryGapInput{MachineID: machine.MachineID, LedgerEpoch: epoch, AgentAllocated: 10, FirstSeq: 6, LastSeq: 10, Receipt: receipt}, now)
	if !errors.Is(detailErr, ErrTrafficGapHasDetails) {
		t.Fatalf("Panel details were treated as a missing gap: %v", detailErr)
	}
}
