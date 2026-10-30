package storage

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
)

func retentionSegment(t *testing.T, f *trafficFixture, up, down int64, final bool, closeSeq uint64) protocol.TrafficSegment {
	t.Helper()
	ordinal := uint64(f.NextID + 1)
	s := f.segment(t, up, down, final, closeSeq, "")
	return bindRetentionSegment(t, s, ordinal)
}

func bindRetentionSegment(t *testing.T, s protocol.TrafficSegment, ordinal uint64) protocol.TrafficSegment {
	t.Helper()
	id, err := protocol.NewTrafficSegmentID(ordinal, s)
	if err != nil {
		t.Fatal(err)
	}
	s.Key.SegmentID = id
	return s
}

func TestRetentionAcrossPeriodsAndSnapshot(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows, r.RawRows, r.MonthMonths = 1, 2, 1
	if err = f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	var first protocol.TrafficSegment
	for month := 1; month <= 5; month++ {
		at := time.Date(2026, time.Month(month), 2, 2, 0, 0, 0, time.UTC)
		f.Current, err = period.At(f.Policy, at)
		if err != nil {
			t.Fatal(err)
		}
		s := retentionSegment(t, f, 11, 13, true, uint64(month))
		if month == 1 {
			first = s
		}
		req := trafficRequest(t, f, s)
		ack, err := f.Store.IngestTraffic(t.Context(), f.Machine.MachineID, req, []protocol.TrafficRow{{Segment: s}}, at)
		if err != nil || ack.Outcomes[0].Status != "retired" {
			t.Fatalf("month %d: %+v %v", month, ack, err)
		}
	}
	var periods int
	if err = f.Store.db.QueryRow(`SELECT COUNT(*) FROM traffic_usage`).Scan(&periods); err != nil || periods > 2 {
		t.Fatalf("unbounded periods: %d %v", periods, err)
	}
	var up, down int
	if err = f.Store.db.QueryRow(`SELECT SUM(uplink),SUM(downlink) FROM traffic_rollups WHERE resolution='month'`).Scan(&up, &down); err != nil || up != 22 || down != 26 {
		t.Fatalf("rollup retention %d/%d %v", up, down, err)
	}
	// A consistent SQLite snapshot must include compacted membership metadata.
	snapshot, err := f.Store.Snapshot(t.Context())
	if err != nil || len(snapshot.Database) == 0 {
		t.Fatalf("snapshot %v", err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err = os.WriteFile(path, snapshot.Database, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var retiredThrough int
	if err = db.QueryRow(`SELECT last FROM traffic_retired_ordinals WHERE first=1`).Scan(&retiredThrough); err != nil || retiredThrough != 4 {
		t.Fatalf("snapshot lost membership: %d %v", retiredThrough, err)
	}
	ack := ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Outcomes[0].Status != "retired" {
		t.Fatalf("old period replay %+v", ack)
	}
}

func TestRetentionLastDayKeepsPreviousCalendarMonth(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.MonthMonths = 1
	if err = f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for month := 1; month <= 3; month++ {
		day := 2
		if month == 3 {
			day = 31
		}
		at := time.Date(2026, time.Month(month), day, 2, 0, 0, 0, time.UTC)
		f.Current, err = period.At(f.Policy, at)
		if err != nil {
			t.Fatal(err)
		}
		s := retentionSegment(t, f, 11, 13, true, uint64(month))
		_, err = f.Store.IngestTraffic(t.Context(), f.Machine.MachineID, trafficRequest(t, f, s), []protocol.TrafficRow{{Segment: s}}, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	var periods, months int
	if err = f.Store.db.QueryRow(`SELECT COUNT(*) FROM traffic_usage`).Scan(&periods); err != nil || periods != 2 {
		t.Fatalf("previous period lost at month end: %d %v", periods, err)
	}
	if err = f.Store.db.QueryRow(`SELECT COUNT(*) FROM traffic_rollups WHERE resolution='month'`).Scan(&months); err != nil || months != 2 {
		t.Fatalf("previous month lost: %d %v", months, err)
	}
}

func TestCompactedFinalReplayDoesNotRebillOrRetireActiveSegment(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows = 1
	if err = f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	first := retentionSegment(t, f, 10, 20, true, 1)
	second := retentionSegment(t, f, 30, 40, true, 2)
	for _, s := range []protocol.TrafficSegment{first, second} {
		ack := ingestTraffic(t, f, trafficRequest(t, f, s), protocol.TrafficRow{Segment: s})
		if ack.Outcomes[0].Status != "retired" {
			t.Fatalf("final not retired: %+v", ack)
		}
	}
	var count int
	if err = f.Store.db.QueryRow(`SELECT COUNT(*) FROM traffic_closed_segments`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("closed detail count=%d err=%v", count, err)
	}
	ack := ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Outcomes[0].Status != "retired" {
		t.Fatalf("compacted replay: %+v", ack)
	}
	for _, change := range []func(*protocol.TrafficSegment){func(s *protocol.TrafficSegment) { s.Start = s.Start.Add(time.Second) }, func(s *protocol.TrafficSegment) {
		p, e := period.New(f.Scope, 2, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
		if e != nil {
			t.Fatal(e)
		}
		replacement, e := period.At(p, s.Start)
		if e != nil {
			t.Fatal(e)
		}
		s.Periods = []period.Period{replacement}
	}} {
		mutated := first
		change(&mutated)
		bad := ingestTraffic(t, f, trafficRequest(t, f, mutated), protocol.TrafficRow{Segment: mutated})
		if bad.Outcomes[0].Status != "rejected" {
			t.Fatalf("compacted binding tamper accepted: %+v", bad)
		}
	}
	up, down := usageFor(t, f)
	if up != 40 || down != 60 {
		t.Fatalf("replay rebilled %d/%d", up, down)
	}
	active := retentionSegment(t, f, 3, 4, false, 0)
	ack = ingestTraffic(t, f, trafficRequest(t, f, active), protocol.TrafficRow{Segment: active})
	if ack.Outcomes[0].Status != "cumulative" {
		t.Fatalf("new active: %+v", ack)
	}
	active.Finalized = true
	active.CloseSeq = 1
	ack = ingestTraffic(t, f, trafficRequest(t, f, active), protocol.TrafficRow{Segment: active})
	if ack.Outcomes[0].Status != "rejected" {
		t.Fatalf("active K falsely retired: %+v", ack)
	}
}

func TestCompactedFinalCannotBeReusedWithNewCloseSequence(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows = 1
	if err := f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	first := retentionSegment(t, f, 10, 20, true, 1)
	second := retentionSegment(t, f, 30, 40, true, 2)
	for _, s := range []protocol.TrafficSegment{first, second} {
		ingestTraffic(t, f, trafficRequest(t, f, s), protocol.TrafficRow{Segment: s})
	}
	first.CloseSeq = 3
	ack := ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Outcomes[0].Status != "rejected" {
		t.Fatalf("compacted K reused with new sequence: %+v", ack.Outcomes[0])
	}
	up, down := usageFor(t, f)
	if up != 40 || down != 60 {
		t.Fatalf("compacted K billed twice: %d/%d", up, down)
	}
}

func TestCompactionDoesNotRejectNewOfflineFinal(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows = 1
	if err := f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	offline := retentionSegment(t, f, 5, 6, true, 3)
	first := retentionSegment(t, f, 10, 20, true, 1)
	second := retentionSegment(t, f, 30, 40, true, 2)
	for _, s := range []protocol.TrafficSegment{first, second} {
		ingestTraffic(t, f, trafficRequest(t, f, s), protocol.TrafficRow{Segment: s})
	}
	ack := ingestTraffic(t, f, trafficRequest(t, f, offline), protocol.TrafficRow{Segment: offline})
	if ack.Outcomes[0].Status != "retired" {
		t.Fatalf("valid offline final rejected: %+v", ack.Outcomes[0])
	}
}

func TestOrdinalCompactionPreservesOfflineHolesAndLegacy(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows, r.RawRows = 1, 2
	if err = f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	legacy := f.segment(t, 7, 8, true, 1, "")
	ingestTraffic(t, f, trafficRequest(t, f, legacy), protocol.TrafficRow{Segment: legacy})
	// Ordinal 1 stays offline while later-created segments finalize first.
	var replay protocol.TrafficSegment
	for ordinal := uint64(2); ordinal <= 30; ordinal++ {
		s := bindRetentionSegment(t, f.segment(t, 1, 2, true, ordinal, ""), ordinal)
		if ordinal == 2 {
			replay = s
		}
		a := ingestTraffic(t, f, trafficRequest(t, f, s), protocol.TrafficRow{Segment: s})
		if a.Outcomes[0].Status != "retired" {
			t.Fatalf("%d: %+v", ordinal, a)
		}
	}
	offline := f.segment(t, 3, 4, true, 31, "")
	// A different stream may close a much older interval after other streams.
	offline.Key.StreamID = mustUUID(t)
	offline.Start, offline.Through = f.Current.Start, f.Current.Start
	offline = bindRetentionSegment(t, offline, 1)
	a := ingestTraffic(t, f, trafficRequest(t, f, offline), protocol.TrafficRow{Segment: offline})
	if a.Outcomes[0].Status != "retired" {
		t.Fatalf("offline: %+v", a)
	}
	for _, original := range []protocol.TrafficSegment{replay, legacy} {
		original.CloseSeq = 32
		a = ingestTraffic(t, f, trafficRequest(t, f, original), protocol.TrafficRow{Segment: original})
		if a.Outcomes[0].Status != "rejected" {
			t.Fatalf("rebill: %+v", a)
		}
	}
	up, down := usageFor(t, f)
	if up != 39 || down != 70 {
		t.Fatalf("totals %d/%d", up, down)
	}
	for table, maximum := range map[string]int{"traffic_retired_ordinals": 1, "traffic_closed_segments": 2, "traffic_reports": 2} {
		var count int
		if err = f.Store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count > maximum {
			t.Fatalf("%s=%d: %v", table, count, err)
		}
	}
}

func TestCompactionFailureRollsBackAccountingAndReceipt(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	r, err := f.Store.GetTrafficRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ClosedRows = 1
	if err = f.Store.SaveTrafficRetention(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	first := retentionSegment(t, f, 10, 20, true, 1)
	second := retentionSegment(t, f, 30, 40, true, 2)
	ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if _, err = f.Store.db.Exec(`CREATE TRIGGER fail_compaction BEFORE INSERT ON traffic_retired_ordinals BEGIN SELECT RAISE(FAIL,'injected compaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	ack, err := f.Store.IngestTraffic(t.Context(), f.Machine.MachineID, trafficRequest(t, f, second), []protocol.TrafficRow{{Segment: second}}, time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC))
	if err == nil || ack.Receipt != nil {
		t.Fatalf("failed transaction acknowledged: %+v %v", ack, err)
	}
	up, down := usageFor(t, f)
	if up != 10 || down != 20 {
		t.Fatalf("failed transaction charged: %d/%d", up, down)
	}
	var closed int
	if err = f.Store.db.QueryRow(`SELECT closed_through FROM traffic_ledgers`).Scan(&closed); err != nil || closed != 1 {
		t.Fatalf("prefix advanced: %d %v", closed, err)
	}
	if _, err = f.Store.db.Exec(`DROP TRIGGER fail_compaction`); err != nil {
		t.Fatal(err)
	}
	ingestTraffic(t, f, trafficRequest(t, f, second), protocol.TrafficRow{Segment: second})
	up, down = usageFor(t, f)
	if up != 40 || down != 60 {
		t.Fatalf("retry: %d/%d", up, down)
	}
}

func TestCreationOrdinalCannotAliasAnotherK(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	first := retentionSegment(t, f, 10, 20, true, 1)
	ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	alias := bindRetentionSegment(t, f.segment(t, 30, 40, true, 2, ""), 1)
	ack := ingestTraffic(t, f, trafficRequest(t, f, alias), protocol.TrafficRow{Segment: alias})
	if ack.Outcomes[0].Status != "rejected" {
		t.Fatalf("ordinal reused before compaction: %+v", ack)
	}
	up, down := usageFor(t, f)
	if up != 10 || down != 20 {
		t.Fatalf("alias billed: %d/%d", up, down)
	}
}

func TestRestoredOrdinalProofRejectsRebillAndAcceptsOfflineHole(t *testing.T) {
	f := newTrafficFixture(t)
	defer f.Store.Close()
	recoverTraffic(t, f, 0, nil, nil)
	first := retentionSegment(t, f, 10, 20, true, 1)
	ack := ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Receipt == nil || ack.Receipt.Version != 2 || ack.Receipt.ClosedOrdinals != "1-1" {
		t.Fatalf("missing identity proof: %+v", ack.Receipt)
	}
	// Simulate a restored backup missing the later acknowledged accounting.
	for _, query := range []string{`DELETE FROM traffic_closed_segments`, `DELETE FROM traffic_retired_ordinals`, `DELETE FROM traffic_usage`, `UPDATE traffic_ledgers SET closed_through=0,reconciliation_pending=1`} {
		if _, err := f.Store.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	active := first
	active.Finalized, active.CloseSeq = false, 0
	active.Totals = protocol.Totals{}
	body, _ := json.Marshal(active)
	if _, err := f.Store.db.Exec(`INSERT INTO traffic_segments(machine_id,ledger_epoch,stream_id,segment_id,body,accepted_up,accepted_down,finalized,close_seq,updated_at) VALUES(?,?,?,?,?,0,0,0,NULL,?)`, first.Key.MachineID, first.Key.LedgerEpoch, first.Key.StreamID, first.Key.SegmentID, string(body), trafficTextTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	input := TrafficRecoveryGapInput{MachineID: f.Machine.MachineID, LedgerEpoch: f.Epoch, FirstSeq: 1, LastSeq: 1, AgentAllocated: 1, Receipt: *ack.Receipt}
	input.Receipt.ClosedOrdinals = "2-2"
	if err := f.Store.RecordTrafficRecoveryGap(t.Context(), input, time.Now().UTC()); err != ErrTrafficGapEvidence {
		t.Fatalf("tampered proof: %v", err)
	}
	input.Receipt = *ack.Receipt
	if err := f.Store.RecordTrafficRecoveryGap(t.Context(), input, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := f.Store.db.QueryRow(`SELECT COUNT(*) FROM traffic_segments`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("proven closed cumulative row retained: %d %v", pending, err)
	}
	first.CloseSeq = 2
	ack = ingestTraffic(t, f, trafficRequest(t, f, first), protocol.TrafficRow{Segment: first})
	if ack.Outcomes[0].Status != "rejected" {
		t.Fatalf("restored K rebilled: %+v", ack)
	}
	offline := retentionSegment(t, f, 5, 6, true, 2)
	offline.Start, offline.Through = f.Current.Start, f.Current.Start
	offline = bindRetentionSegment(t, offline, protocol.TrafficOrdinal(offline.Key.SegmentID))
	ack = ingestTraffic(t, f, trafficRequest(t, f, offline), protocol.TrafficRow{Segment: offline})
	if ack.Outcomes[0].Status != "retired" {
		t.Fatalf("offline hole rejected: %+v", ack)
	}
	up, down := usageFor(t, f)
	if up != 5 || down != 6 {
		t.Fatalf("invented lost usage or rebill: %d/%d", up, down)
	}
}
