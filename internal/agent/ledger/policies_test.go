package ledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestOfflinePolicyApplicationPreservesOldFinalAndGlobalPeriod(t *testing.T) {
	ctx := context.Background()
	s := openBook(t, t.TempDir())
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-02-01T00:00:00Z"), true))
	old := sample("2024-02-09T00:00:00Z", 100)
	must(t, s.Ingest(ctx, old))
	// The remote change at the 10th remains unknown until the 14th. The final
	// old-policy sample is committed before applying the newly received policy.
	last := sample("2024-02-14T00:00:00Z", 150)
	last.Finalize = true
	must(t, s.Ingest(ctx, last))
	recoverBook(t, s, nil)
	before := report(t, s)
	frozen, _ := json.Marshal(before.Segments[0])
	next := sample("2024-02-14T00:00:05Z", 200)
	next.PolicyAppliedAt = last.Finished
	changed := next.Policies[0]
	changed.ID, _ = security.NewUUID()
	changed.EffectiveAt = instant("2024-02-10T12:00:00Z")
	next.Policies = append(next.Policies, changed)
	must(t, s.Ingest(ctx, next))
	after := report(t, s)
	again, _ := json.Marshal(after.Segments[0])
	if string(frozen) != string(again) || len(after.Segments) != 2 || after.Segments[1].Totals.Uplink != 50 || !after.Segments[1].Start.Equal(last.Finished) {
		t.Fatal("offline old segment reassigned", after)
	}
	for _, p := range after.Segments[1].Periods {
		if p.Scope.Kind == "user" && (p.PolicyID != changed.ID || p.PolicyRevision != changed.Revision || !p.Start.Equal(changed.EffectiveAt)) {
			t.Fatal("machine apply point changed global period", p)
		}
	}
	// Reusing a policy ID with other content fails the entire sample atomically.
	bad := next
	bad.Policies = append([]period.Policy(nil), next.Policies...)
	bad.Policies[len(bad.Policies)-1].Monthly.Day = 2
	if s.Ingest(ctx, bad) == nil {
		t.Fatal("published policy overwritten")
	}
}

func TestTransitAndMissingDatabaseAreNeverAttributedOrRebuilt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openBook(t, dir)
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
	v := sample("2024-01-01T00:00:05Z", 100)
	v.Readings[0].Stream = protocol.TrafficStream{Kind: "machine_only_transit"}
	v.Policies = v.Policies[2:]
	must(t, s.Ingest(ctx, v))
	recoverBook(t, s, nil)
	r := report(t, s)
	if len(r.Segments) != 1 || len(r.Segments[0].Periods) != 1 || r.Segments[0].Periods[0].Scope.Kind != "machine" {
		t.Fatal("transit charged user")
	}
	must(t, s.Close())
	must(t, os.Remove(filepath.Join(dir, "agent.ledger.sqlite")))
	if reopened, err := Open(dir, machine); err == nil {
		reopened.Close()
		t.Fatal("missing database silently created another ledger")
	}
}

func TestRecoveryRequestCannotBeMutatedOrReusedAfterPause(t *testing.T) {
	ctx := context.Background()
	s := openBook(t, t.TempDir())
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
	v := sample("2024-01-01T00:00:05Z", 100)
	v.Finalize = true
	must(t, s.Ingest(ctx, v))
	req, err := s.RecoveryRequest(ctx, authority)
	must(t, err)
	req.Held[0].Last++
	res := protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true}
	if s.ConfirmRecovery(ctx, req, res) == nil {
		t.Fatal("mutated recovery request accepted")
	}
	req, err = s.RecoveryRequest(ctx, authority)
	must(t, err)
	res.RequestID = req.RequestID
	s.PauseACKs()
	if s.ConfirmRecovery(ctx, req, res) == nil {
		t.Fatal("paused recovery accepted")
	}
}

func TestHistoricalBoundaryReplayKeepsNewerWatermark(t *testing.T) {
	ctx := context.Background()
	s := openBook(t, t.TempDir())
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
	must(t, s.Ingest(ctx, sample("2024-01-01T00:00:05Z", 100)))
	must(t, s.Ingest(ctx, sample("2024-01-01T00:00:10Z", 200)))
	orphan := sample("2024-01-01T00:00:07Z", 150)
	orphan.ImportID = "orphan-snapshot"
	orphan.Finalize = true
	orphan.HistoricalBoundary = true
	must(t, s.Ingest(ctx, orphan))
	must(t, s.Ingest(ctx, orphan))
	recoverBook(t, s, nil)
	r := report(t, s)
	if len(r.Segments) != 1 || r.Segments[0].CloseSeq != 1 || r.Segments[0].Totals.Uplink != 200 {
		t.Fatal("orphan snapshot changed durable totals", r)
	}
	latest := sample("2024-01-01T00:00:15Z", 250)
	must(t, s.Ingest(ctx, latest))
	r = report(t, s)
	if len(r.Segments) != 2 || r.Segments[1].Totals.Uplink != 50 {
		t.Fatal("orphan snapshot rewound source", r)
	}
	d, err := Inspect(ctx, filepath.Dir(s.path))
	must(t, err)
	if len(d.PendingPreview) != 2 {
		t.Fatal("read-only live ledger diagnostics missing")
	}
}

func TestCumulativeACKStaysBelowLocalTotalsAndDoesNotAuthorizeCleanup(t *testing.T) {
	ctx := context.Background()
	s := openBook(t, t.TempDir())
	defer s.Close()
	must(t, s.RegisterSource(ctx, source, 1, instant("2024-01-01T00:00:00Z"), true))
	must(t, s.Ingest(ctx, sample("2024-01-01T00:00:05Z", 100)))
	recoverBook(t, s, nil)
	r := report(t, s)
	must(t, s.Ingest(ctx, sample("2024-01-01T00:00:10Z", 200)))
	bad := ack(r, nil)
	bad.Outcomes[0].Committed.Uplink = 201
	if s.ConsumeACK(ctx, r, bad, instant("2024-01-01T00:00:11Z")) == nil {
		t.Fatal("ACK exceeded durable local total")
	}
	good := ack(r, nil)
	good.Outcomes[0].Committed.Uplink = 150
	good.Outcomes[0].Committed.Downlink = 150
	must(t, s.ConsumeACK(ctx, r, good, instant("2024-01-01T00:00:11Z")))
	d, err := s.Diagnostic(ctx)
	must(t, err)
	if d.Pending != 1 || d.Metadata.Receipt != nil {
		t.Fatal("ordinary cumulative ACK cleaned a live segment")
	}
	final := sample("2024-01-01T00:00:15Z", 250)
	final.Finalize = true
	must(t, s.Ingest(ctx, final))
	req, err := s.RecoveryRequest(ctx, authority)
	must(t, err)
	res := protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true, Receipt: receipt(t, req.LedgerEpoch, 1)}
	if s.ConfirmRecovery(ctx, req, res) == nil {
		t.Fatal("unreported final segment retired by forged prefix")
	}
	d, err = s.Diagnostic(ctx)
	must(t, err)
	if d.Pending != 1 || d.Metadata.Receipt != nil {
		t.Fatal("forged prefix deleted local data")
	}
}
