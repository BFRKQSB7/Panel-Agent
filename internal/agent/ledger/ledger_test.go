package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

const machine = "11111111-1111-4111-8111-111111111111"
const source = "22222222-2222-4222-8222-222222222222"
const user = "33333333-3333-4333-8333-333333333333"
const node = "44444444-4444-4444-8444-444444444444"
const authority = "55555555-5555-4555-8555-555555555555"
const installation = "66666666-6666-4666-8666-666666666666"

func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func openBook(t *testing.T, dir string) *Store {
	t.Helper()
	must(t, os.Chmod(dir, 0o700))
	s, err := Open(dir, machine)
	must(t, err)
	return s
}
func sample(at string, up int64) Sample {
	when := instant(at)
	scopes := []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: node}, {Kind: "machine", MachineID: machine}}
	ps := []period.Policy{}
	for i, day := range []int{1, 15, 31} {
		ps = append(ps, period.Policy{ID: []string{user, node, machine}[i], Scope: scopes[i], Revision: 1, EffectiveAt: time.Unix(0, 0).UTC(), Monthly: period.DefaultMonthly(day)})
	}
	return Sample{SourceID: source, Generation: 1, Started: when.Add(-time.Millisecond), Finished: when, Policies: ps, Readings: []Reading{{Label: "user-test", Stream: protocol.TrafficStream{Kind: "user_entry", UserID: user, EntryNodeID: node}, Totals: protocol.Totals{Uplink: quantity.Bytes(up), Downlink: quantity.Bytes(up)}}}}
}
func recoverBook(t *testing.T, s *Store, receipt *security.ClosedReceipt) {
	t.Helper()
	req, err := s.RecoveryRequest(context.Background(), authority)
	must(t, err)
	must(t, s.ConfirmRecovery(context.Background(), req, protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true, Receipt: receipt}))
}
func report(t *testing.T, s *Store) protocol.TrafficRequest {
	t.Helper()
	r, err := s.Pending(context.Background(), authority, instant("2027-01-01T00:00:00Z"))
	must(t, err)
	return r
}
func ack(r protocol.TrafficRequest, receipt *security.ClosedReceipt) protocol.TrafficACK {
	a := protocol.TrafficACK{Version: 1, RequestID: r.RequestID, AuthorityEpoch: r.AuthorityEpoch, MachineID: r.MachineID, LedgerEpoch: r.LedgerEpoch, Receipt: receipt}
	for i, s := range r.Segments {
		totals := s.Totals
		status := "cumulative"
		if s.Finalized {
			status = "final"
		}
		a.Outcomes = append(a.Outcomes, protocol.TrafficOutcome{Row: i, Key: s.Key, Status: status, CloseSeq: s.CloseSeq, Committed: &totals})
	}
	return a
}
func receipt(t *testing.T, epoch string, n uint64) *security.ClosedReceipt {
	t.Helper()
	r, err := security.SignClosedReceipt(bytes.Repeat([]byte{8}, 32), security.ClosedClaims{Version: 1, InstallationID: installation, MachineID: machine, LedgerEpoch: epoch, ClosedThrough: n})
	must(t, err)
	return &r
}

func TestAtomicSourceWatermarkGenerationAndDiskFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openBook(t, dir)
	var schemaVersion int
	must(t, s.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion))
	if schemaVersion != 2 {
		t.Fatal("old readers could overwrite creation metadata")
	}
	_, err := s.db.Exec("PRAGMA user_version=3")
	must(t, err)
	if _, err = Inspect(ctx, dir); !errors.Is(err, ErrLedger) {
		t.Fatalf("future ledger accepted by candidate check: %v", err)
	}
	_, err = s.db.Exec("PRAGMA user_version=2")
	must(t, err)
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-30T00:00:00Z"), true))
	first := sample("2024-01-30T00:00:05Z", 100)
	must(t, s.Ingest(ctx, first))
	s.Close()
	s = openBook(t, dir)
	defer s.Close()
	must(t, s.Ingest(ctx, first))
	recoverBook(t, s, nil)
	r := report(t, s)
	if len(r.Segments) != 1 || protocol.TrafficOrdinal(r.Segments[0].Key.SegmentID) != 1 {
		t.Fatalf("creation ordinal lost after restart: %+v", r)
	}
	if len(r.Segments) != 1 || r.Segments[0].Totals.Uplink != 100 {
		t.Fatal("restart/replay double charged")
	}
	epoch := r.LedgerEpoch
	s.beforeCommit = func() error { return errors.New("injected full disk") }
	next := sample("2024-01-30T00:00:10Z", 200)
	if s.Ingest(ctx, next) == nil {
		t.Fatal("disk failure ignored")
	}
	s.beforeCommit = nil
	r = report(t, s)
	if r.Segments[0].Totals.Uplink != 100 {
		t.Fatal("uncommitted watermark advanced")
	}
	must(t, s.db.PingContext(ctx))
	_, err = s.db.Exec("PRAGMA query_only=ON")
	must(t, err)
	if s.Ingest(ctx, next) == nil {
		t.Fatal("SQLite write rejection ignored")
	}
	_, err = s.db.Exec("PRAGMA query_only=OFF")
	must(t, err)
	r = report(t, s)
	if r.Segments[0].Totals.Uplink != 100 {
		t.Fatal("failed SQLite write advanced watermark")
	}
	must(t, s.Ingest(ctx, next))
	must(t, s.Ingest(ctx, next))
	backward := next
	backward.Readings[0].Totals.Uplink = 1
	if s.Ingest(ctx, backward) == nil {
		t.Fatal("same instance counter decrease accepted")
	}
	newSource, _ := security.NewUUID()
	must(t, s.RegisterSource(ctx, newSource, 2, next.Finished, true))
	next.SourceID = newSource
	next.Generation = 2
	next.Readings[0].Totals = protocol.Totals{Uplink: 1000, Downlink: 1000}
	must(t, s.Ingest(ctx, next))
	r = report(t, s)
	if r.LedgerEpoch != epoch || r.Segments[0].Totals.Uplink != 1200 {
		t.Fatal("high new Core counter mistaken for continuity", r)
	}
	unknown, _ := security.NewUUID()
	next.SourceID = unknown
	next.Generation = 3
	next.Readings[0].Totals = protocol.Totals{Uplink: 5000, Downlink: 5000}
	must(t, s.Ingest(ctx, next))
	r = report(t, s)
	if r.Segments[0].Totals.Uplink != 1200 {
		t.Fatal("unproven instance rebilled history")
	}
	var synchronous int
	must(t, s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous))
	if synchronous != 2 {
		t.Fatal("SQLite FULL durability not enabled")
	}
}

func TestOfflineBoundariesFrozenBindingAndClockRollback(t *testing.T) {
	ctx := context.Background()
	s := openBook(t, t.TempDir())
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-30T00:00:00Z"), true))
	must(t, s.Ingest(ctx, sample("2024-01-30T00:00:05Z", 100)))
	late := sample("2024-03-02T00:00:00Z", 150)
	must(t, s.Ingest(ctx, late))
	recoverBook(t, s, nil)
	r := report(t, s)
	if len(r.Segments) != 2 || !r.Segments[0].Finalized || r.Segments[0].CloseSeq != 1 || r.Segments[0].Totals.Uplink != 100 || r.Segments[1].Totals.Uplink != 50 || r.Segments[1].Uncertainty == nil || !r.Segments[1].Uncertainty.From.Equal(instant("2024-01-30T00:00:05Z")) {
		t.Fatal("offline attribution or interval lost", r)
	}
	old, _ := json.Marshal(r.Segments[0])
	currentID := r.Segments[1].Key
	back := sample("2024-02-01T00:00:00Z", 160)
	must(t, s.Ingest(ctx, back))
	r = report(t, s)
	again, _ := json.Marshal(r.Segments[0])
	if !bytes.Equal(old, again) || r.Segments[1].Key != currentID || r.Segments[1].Totals.Uplink != 60 {
		t.Fatal("clock reopened closed period")
	}
	manual := sample("2024-03-02T00:00:05Z", 170)
	manual.Finalize = true
	manual.ImportID = "fixed-boundary"
	must(t, s.Ingest(ctx, manual))
	must(t, s.Ingest(ctx, manual))
	r = report(t, s)
	if len(r.Segments) != 2 || r.Segments[1].CloseSeq != 2 || r.Segments[1].Totals.Uplink != 70 {
		t.Fatal("final sample replay changed final")
	}
}

func TestACKRecoveryPartialFailurePrefixAndNoForgedCleanup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openBook(t, dir)
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
	for _, when := range []string{"2024-01-01T00:00:05Z", "2024-01-01T00:00:10Z"} {
		v := sample(when, 100)
		v.Finalize = true
		must(t, s.Ingest(ctx, v))
	}
	if _, err := s.Pending(ctx, authority, time.Now()); err == nil {
		t.Fatal("sent before recovery handshake")
	}
	recoverBook(t, s, nil)
	r := report(t, s)
	for _, mutate := range []func(*protocol.TrafficACK){func(a *protocol.TrafficACK) { a.RequestID = source }, func(a *protocol.TrafficACK) { a.AuthorityEpoch = source }, func(a *protocol.TrafficACK) { a.Outcomes[0].Committed.Uplink++ }, func(a *protocol.TrafficACK) { a.Receipt = receipt(t, r.LedgerEpoch, 3) }} {
		a := ack(r, nil)
		mutate(&a)
		if s.ConsumeACK(ctx, r, a, time.Now()) == nil {
			t.Fatal("forged ACK accepted")
		}
	}
	a := ack(r, nil)
	a.Outcomes[0] = protocol.TrafficOutcome{Row: 0, Key: r.Segments[0].Key, Status: "rejected", ErrorCode: "unknown_policy"}
	must(t, s.ConsumeACK(ctx, r, a, instant("2026-01-01T00:00:00Z")))
	d, err := s.Diagnostic(ctx)
	must(t, err)
	if d.Pending != 2 || d.Metadata.Receipt != nil {
		t.Fatal("partial ACK deleted gap")
	}
	r = report(t, s)
	a = ack(r, receipt(t, r.LedgerEpoch, 2))
	s.beforeCommit = func() error { return errors.New("ACK commit failure") }
	if s.ConsumeACK(ctx, r, a, time.Now()) == nil {
		t.Fatal("ACK failure ignored")
	}
	s.beforeCommit = nil
	d, err = s.Diagnostic(ctx)
	must(t, err)
	if d.Pending != 2 || d.Metadata.Receipt != nil {
		t.Fatal("ACK removed uncommitted data")
	}
	for i := range a.Outcomes {
		a.Outcomes[i].Status = "retired"
		a.Outcomes[i].Committed = nil
	}
	must(t, s.ConsumeACK(ctx, r, a, time.Now()))
	d, err = s.Diagnostic(ctx)
	must(t, err)
	if d.Pending != 0 || d.Metadata.Receipt == nil || d.Metadata.Receipt.ClosedThrough != 2 || d.Metadata.Allocated != 2 {
		t.Fatal("cleanup lost recovery proof")
	}
	s.PauseACKs()
	if s.ConsumeACK(ctx, r, a, time.Now()) == nil {
		t.Fatal("late ACK after authority invalidation")
	}
	req, err := s.RecoveryRequest(ctx, authority)
	must(t, err)
	res := protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true}
	if s.ConfirmRecovery(ctx, req, res) == nil {
		t.Fatal("old backup prefix rollback silently accepted")
	}
	res.Receipt = d.Metadata.Receipt
	must(t, s.ConfirmRecovery(ctx, req, res))
	next := sample("2024-01-01T00:00:15Z", 150)
	next.Finalize = true
	must(t, s.Ingest(ctx, next))
	r = report(t, s)
	if r.Segments[0].CloseSeq != 3 {
		t.Fatal("reused retired sequence")
	}
}

func TestLedgerSubprocessCrashDurability(t *testing.T) {
	for _, mode := range []string{"sample_before", "sample_after", "ack_before", "ack_after"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s := openBook(t, dir)
			ctx := context.Background()
			must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
			first := sample("2024-01-01T00:00:05Z", 100)
			must(t, s.Ingest(ctx, first))
			s.Close()
			exe, err := os.Executable()
			must(t, err)
			cmd := exec.Command(exe, "-test.run=^TestLedgerCrashHelper$")
			cmd.Env = append(os.Environ(), "D08_CRASH_DIR="+dir, "D08_CRASH_MODE="+mode)
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Fatal("crash helper did not reach failpoint", err)
			}
			s = openBook(t, dir)
			defer s.Close()
			d, err := s.Diagnostic(ctx)
			must(t, err)
			if mode == "ack_after" {
				if d.Pending != 0 || d.Metadata.Receipt == nil {
					t.Fatal("committed ACK cleanup/proof not recovered")
				}
				return
			}
			recoverBook(t, s, nil)
			r := report(t, s)
			want := quantity.Bytes(100)
			if mode == "sample_after" || mode == "ack_before" {
				want = 200
			}
			if len(r.Segments) != 1 || r.Segments[0].Totals.Uplink != want {
				t.Fatal("crash changed durability boundary", r)
			}
			if mode == "sample_before" || mode == "sample_after" {
				must(t, s.Ingest(ctx, sample("2024-01-01T00:00:10Z", 200)))
				r = report(t, s)
				if r.Segments[0].Totals.Uplink != 200 {
					t.Fatal("retry double counted")
				}
			}
		})
	}
}
func TestLedgerCrashHelper(t *testing.T) {
	dir := os.Getenv("D08_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}
	mode := os.Getenv("D08_CRASH_MODE")
	s := openBook(t, dir)
	ctx := context.Background()
	v := sample("2024-01-01T00:00:10Z", 200)
	if mode == "sample_before" {
		s.beforeCommit = func() error { os.Exit(91); return nil }
	}
	if mode == "sample_after" {
		s.afterCommit = func() { os.Exit(91) }
	}
	if mode == "ack_before" || mode == "ack_after" {
		v.Finalize = true
	}
	must(t, s.Ingest(ctx, v))
	recoverBook(t, s, nil)
	r := report(t, s)
	if mode == "ack_before" {
		s.beforeCommit = func() error { os.Exit(91); return nil }
	}
	if mode == "ack_after" {
		s.afterCommit = func() { os.Exit(91) }
	}
	must(t, s.ConsumeACK(ctx, r, ack(r, receipt(t, r.LedgerEpoch, 1)), time.Now()))
	t.Fatal("failpoint not reached")
}
