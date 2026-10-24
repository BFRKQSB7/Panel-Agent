package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

type trafficFixture struct {
	Store     *Store
	Machine   MachineRecord
	Authority PanelAuthority
	Epoch     string
	Install   string
	Scope     period.Scope
	Policy    period.Policy
	Current   period.Period
	StreamID  string
	NextID    int
}

func newTrafficFixture(t *testing.T) *trafficFixture {
	t.Helper()
	s := openMachineTestStore(t)
	ctx := context.Background()
	if _, err := s.InitializeAdmin(ctx, "traffic-test-password-hash"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	join, candidate, registration := testSecrets(t)
	if err := s.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, authority, err := s.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	install, err := readInstallationForTest(s)
	if err != nil {
		t.Fatal(err)
	}
	scope := period.Scope{Kind: "machine", MachineID: machine.MachineID}
	policy, err := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(policy)
	if _, err := s.db.Exec(`INSERT INTO period_policies(policy_id,scope_key,revision,effective_at,policy_json) VALUES(?,?,?,?,?)`,
		policy.ID, scope.Key(), policy.Revision, policy.EffectiveAt.Format(time.RFC3339Nano), string(encoded)); err != nil {
		t.Fatal(err)
	}
	current, err := period.At(policy, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return &trafficFixture{Store: s, Machine: machine, Authority: authority, Epoch: mustUUID(t), Install: install,
		Scope: scope, Policy: policy, Current: current, StreamID: stream}
}

func readInstallationForTest(s *Store) (string, error) {
	var installation string
	err := s.db.QueryRow("SELECT installation_id FROM installation WHERE id=1").Scan(&installation)
	return installation, err
}

func mustUUID(t *testing.T) string {
	t.Helper()
	id, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *trafficFixture) segment(t *testing.T, up, down int64, finalized bool, closeSeq uint64, id string) protocol.TrafficSegment {
	t.Helper()
	if id == "" {
		id = mustUUID(t)
	}
	start := f.Current.Start.Add(time.Duration(f.NextID) * time.Minute)
	f.NextID++
	segment := protocol.TrafficSegment{
		Key:       protocol.SegmentKey{MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, StreamID: f.StreamID, SegmentID: id},
		Stream:    protocol.TrafficStream{Kind: "machine_only_transit"},
		Periods:   []period.Period{f.Current},
		Start:     start,
		Through:   start.Add(time.Minute),
		Totals:    protocol.Totals{Uplink: quantity.Bytes(up), Downlink: quantity.Bytes(down)},
		Finalized: finalized,
		CloseSeq:  closeSeq,
	}
	if err := segment.Validate(); err != nil {
		t.Fatal(err)
	}
	return segment
}

func trafficRequest(t *testing.T, f *trafficFixture, segments ...protocol.TrafficSegment) protocol.TrafficRequest {
	t.Helper()
	requestID := mustUUID(t)
	return protocol.TrafficRequest{Version: protocol.TrafficVersion, RequestID: requestID, AuthorityEpoch: f.Authority.Epoch,
		MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, Segments: segments}
}

func recoverTraffic(t *testing.T, f *trafficFixture, allocated uint64, held []protocol.CloseRange, previous *security.ClosedReceipt) protocol.TrafficRecoveryResponse {
	t.Helper()
	request := protocol.TrafficRecoveryRequest{Version: protocol.TrafficVersion, RequestID: mustUUID(t), AuthorityEpoch: f.Authority.Epoch,
		MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, InstallationID: f.Install, Allocated: allocated, Held: held, Receipt: previous}
	response, err := f.Store.RecoverTraffic(context.Background(), f.Machine.MachineID, request, time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func ingestTraffic(t *testing.T, f *trafficFixture, request protocol.TrafficRequest, rows ...protocol.TrafficRow) protocol.TrafficACK {
	t.Helper()
	ack, err := f.Store.IngestTraffic(context.Background(), f.Machine.MachineID, request, rows, time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func usageFor(t *testing.T, f *trafficFixture) (int64, int64) {
	t.Helper()
	var up, down int64
	err := f.Store.db.QueryRow(`SELECT uplink,downlink FROM traffic_usage WHERE machine_id=? AND ledger_epoch=? AND scope_key=? AND period_id=?`,
		f.Machine.MachineID, f.Epoch, f.Scope.Key(), f.Current.ID).Scan(&up, &down)
	if err != nil {
		t.Fatal(err)
	}
	return up, down
}

func TestTrafficIngestIsIdempotentAndClosesOnlyContiguousFinalPrefix(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recovery := protocol.TrafficRecoveryRequest{Version: protocol.TrafficVersion, RequestID: mustUUID(t), AuthorityEpoch: f.Authority.Epoch,
		MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch}
	if response, err := f.Store.RecoverTraffic(context.Background(), f.Machine.MachineID, recovery, time.Now()); err != nil || !response.Resume || response.InstallationID != f.Install {
		t.Fatalf("initial traffic recovery = (%+v, %v)", response, err)
	}
	segmentID := mustUUID(t)
	first := f.segment(t, 100, 40, false, 0, segmentID)
	request := trafficRequest(t, f, first)
	ack := ingestTraffic(t, f, request, protocol.TrafficRow{Segment: first})
	if len(ack.Outcomes) != 1 || ack.Outcomes[0].Status != "cumulative" || ack.Outcomes[0].Committed.Uplink != 100 {
		t.Fatalf("first cumulative ACK = %+v", ack)
	}
	second := first
	second.Totals = protocol.Totals{Uplink: 150, Downlink: 50}
	second.Through = second.Through.Add(time.Minute)
	ack = ingestTraffic(t, f, trafficRequest(t, f, second), protocol.TrafficRow{Segment: second})
	if ack.Outcomes[0].Status != "cumulative" || ack.Outcomes[0].Committed.Uplink != 150 {
		t.Fatalf("watermark advance ACK = %+v", ack)
	}
	ack = ingestTraffic(t, f, trafficRequest(t, f, second), protocol.TrafficRow{Segment: second})
	if ack.Outcomes[0].Status != "cumulative" {
		t.Fatalf("duplicate cumulative ACK = %+v", ack)
	}
	up, down := usageFor(t, f)
	if up != 150 || down != 50 {
		t.Fatalf("duplicate report rebilled usage: %d/%d", up, down)
	}
	lower := second
	lower.Totals = protocol.Totals{Uplink: 149, Downlink: 50}
	ack = ingestTraffic(t, f, trafficRequest(t, f, lower), protocol.TrafficRow{Segment: lower})
	if ack.Outcomes[0].Status != "rejected" || ack.Outcomes[0].ErrorCode != trafficErrorSegmentConflict {
		t.Fatalf("decreasing watermark was accepted: %+v", ack)
	}

	final := second
	final.Finalized, final.CloseSeq = true, 1
	ack = ingestTraffic(t, f, trafficRequest(t, f, final), protocol.TrafficRow{Segment: final})
	if ack.Outcomes[0].Status != "retired" || ack.Receipt == nil || ack.Receipt.ClosedThrough != 1 {
		t.Fatalf("closed final ACK = %+v", ack)
	}
	ack = ingestTraffic(t, f, trafficRequest(t, f, final), protocol.TrafficRow{Segment: final})
	if ack.Outcomes[0].Status != "retired" || ack.Receipt == nil || ack.Receipt.ClosedThrough != 1 {
		t.Fatalf("replayed retired ACK = %+v", ack)
	}
	var count int
	if err := f.Store.db.QueryRow("SELECT COUNT(*) FROM traffic_segments").Scan(&count); err != nil || count != 0 {
		t.Fatalf("closed detail was not compacted: count=%d err=%v", count, err)
	}
	up, down = usageFor(t, f)
	if up != 150 || down != 50 {
		t.Fatalf("final replay rebilled usage: %d/%d", up, down)
	}
	if err := f.Store.db.QueryRow("SELECT COUNT(*) FROM traffic_reports").Scan(&count); err != nil || count != 6 {
		t.Fatalf("raw traffic reports were not retained: count=%d err=%v", count, err)
	}
}

func TestTrafficBadRowsDoNotBlockGoodRowsAndUnknownEpochIsRejected(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	good := f.segment(t, 20, 30, false, 0, "")
	bad := good
	bad.Periods = append([]period.Period(nil), good.Periods...)
	bad.Key.SegmentID = mustUUID(t)
	unknownPolicy, err := period.New(f.Scope, 2, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
	if err != nil {
		t.Fatal(err)
	}
	bad.Periods[0], err = period.At(unknownPolicy, bad.Start)
	if err != nil {
		t.Fatal(err)
	}
	request := trafficRequest(t, f, good, bad)
	ack := ingestTraffic(t, f, request,
		protocol.TrafficRow{Segment: good},
		protocol.TrafficRow{Segment: bad},
	)
	if len(ack.Outcomes) != 2 || ack.Outcomes[0].Status != "cumulative" || ack.Outcomes[1].Status != "rejected" || ack.Outcomes[1].ErrorCode != trafficErrorUnknownPolicy {
		t.Fatalf("row-isolated ACK = %+v", ack)
	}
	up, down := usageFor(t, f)
	if up != 20 || down != 30 {
		t.Fatalf("good row did not commit independently: %d/%d", up, down)
	}
	unknown := request
	unknown.LedgerEpoch = mustUUID(t)
	unknownSegment := good
	unknownSegment.Key.LedgerEpoch = unknown.LedgerEpoch
	unknown.Segments = []protocol.TrafficSegment{unknownSegment}
	if _, err := f.Store.IngestTraffic(context.Background(), f.Machine.MachineID, unknown, []protocol.TrafficRow{{Segment: unknownSegment}}, time.Now()); !errors.Is(err, ErrTrafficEpoch) {
		t.Fatalf("unknown ledger epoch result = %v", err)
	}
}

func TestTrafficRecoveryRequiresHeldFinalsAndSupportsOutOfOrderClosure(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	second := f.segment(t, 10, 10, true, 2, "")
	ack := ingestTraffic(t, f, trafficRequest(t, f, second), protocol.TrafficRow{Segment: second})
	if ack.Outcomes[0].Status != "final" || ack.Receipt != nil {
		t.Fatalf("out-of-order final ACK = %+v", ack)
	}
	missing := protocol.TrafficRecoveryRequest{Version: protocol.TrafficVersion, RequestID: mustUUID(t), AuthorityEpoch: f.Authority.Epoch,
		MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, InstallationID: f.Install, Allocated: 2}
	if _, err := f.Store.RecoverTraffic(context.Background(), f.Machine.MachineID, missing, time.Now()); !errors.Is(err, ErrTrafficRecoveryGap) {
		t.Fatalf("missing held final result = %v", err)
	}
	var pending int
	if err := f.Store.db.QueryRow("SELECT reconciliation_pending FROM traffic_ledgers WHERE machine_id=?", f.Machine.MachineID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("recovery gap did not freeze reconciliation: %d/%v", pending, err)
	}
	withHeld := missing
	withHeld.Held = []protocol.CloseRange{{First: 2, Last: 2}}
	if response, err := f.Store.RecoverTraffic(context.Background(), f.Machine.MachineID, withHeld, time.Now()); err != nil || !response.Resume {
		t.Fatalf("held final recovery = (%+v, %v)", response, err)
	}
	if err := f.Store.db.QueryRow("SELECT reconciliation_pending FROM traffic_ledgers WHERE machine_id=?", f.Machine.MachineID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("successful recovery did not clear reconciliation: %d/%v", pending, err)
	}
	first := f.segment(t, 10, 10, true, 1, "")
	ack = ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Outcomes[0].Status != "retired" || ack.Receipt == nil || ack.Receipt.ClosedThrough != 2 {
		t.Fatalf("gap-closing ACK = %+v", ack)
	}
	var closed int64
	if err := f.Store.db.QueryRow("SELECT closed_through FROM traffic_ledgers WHERE machine_id=?", f.Machine.MachineID).Scan(&closed); err != nil || closed != 2 {
		t.Fatalf("closed prefix = %d err=%v", closed, err)
	}
}

func TestTrafficInvalidReceiptAndFailedDatabaseWriteDoNotAdvanceState(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	segment := f.segment(t, 1, 2, false, 0, "")
	request := trafficRequest(t, f, segment)
	badReceipt, err := security.SignClosedReceipt(make([]byte, security.MasterKeySize), security.ClosedClaims{Version: 1,
		InstallationID: f.Install, MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, ClosedThrough: 1})
	if err != nil {
		t.Fatal(err)
	}
	request.PreviousReceipt = &badReceipt
	if _, err := f.Store.IngestTraffic(context.Background(), f.Machine.MachineID, request, []protocol.TrafficRow{{Segment: segment}}, time.Now()); !errors.Is(err, ErrTrafficReceipt) {
		t.Fatalf("forged receipt result = %v", err)
	}
	var count int
	if err := f.Store.db.QueryRow("SELECT COUNT(*) FROM traffic_segments").Scan(&count); err != nil || count != 0 {
		t.Fatalf("forged receipt changed state: count=%d err=%v", count, err)
	}
	if err := f.Store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.IngestTraffic(context.Background(), f.Machine.MachineID, trafficRequest(t, f, segment), []protocol.TrafficRow{{Segment: segment}}, time.Now()); err == nil {
		t.Fatal("database failure was reported as a successful ACK")
	}
}
