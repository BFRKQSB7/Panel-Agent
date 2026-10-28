package agent

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestHysteriaSourceCrashReplayAndFinalRetention(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	machine, _ := security.NewUUID()
	user, _ := security.NewUUID()
	nodeID, _ := security.NewUUID()
	credential, _ := security.NewUUID()
	authority, _ := security.NewUUID()
	installation, _ := security.NewUUID()
	book, err := ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	node := protocol.DesiredNode{ID: nodeID, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: credential, Email: "user-node"}}}
	policies := []period.Policy{}
	for _, scope := range []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: nodeID}, {Kind: "machine", MachineID: machine}} {
		p, err := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, p)
	}
	at := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	source, err := newHysteriaSource(ctx, dir, book, node, policies, 1, at)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(credential)) {
		t.Fatal("credential leaked into accounting journal")
	}
	first := map[string]hysteria.Counter{"user-node": {TX: 100, RX: 200}}
	if err := source.record(ctx, first, at.Add(time.Second), false, ""); err != nil {
		t.Fatal(err)
	}
	first["user-node"] = hysteria.Counter{TX: 0}
	if source.state.Last["user-node"].TX != 100 {
		t.Fatal("caller mutated committed observation")
	}
	// Persist a newer observation, then simulate death after ledger commit but
	// before the source journal can acknowledge it. Replay must not charge twice.
	importID, _ := security.NewUUID()
	next := source.state
	next.Pending = &hysteriaObservation{ID: importID, At: at.Add(2 * time.Second), Counters: map[string]hysteria.Counter{"user-node": {TX: 150, RX: 250}}}
	if err := source.save(next); err != nil {
		t.Fatal(err)
	}
	readings, err := hysteriaReadings(nodeID, next.Mappings, next.Pending.Counters)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Ingest(ctx, ledger.Sample{ImportID: importID, SourceID: next.SourceID, Generation: 1, Started: at, Finished: next.Pending.At, PolicyAppliedAt: at, Policies: policies, Readings: readings, HistoricalBoundary: true}); err != nil {
		t.Fatal(err)
	}
	source, err = loadHysteriaSource(ctx, source.path, book)
	if err != nil {
		t.Fatal(err)
	}
	if source.state.Last["user-node"].TX != 150 || source.state.Pending != nil || source.state.CleanupImport != "" {
		t.Fatal("pending observation not recovered")
	}
	if err := source.record(ctx, map[string]hysteria.Counter{}, at.Add(3*time.Second), false, ""); err == nil {
		t.Fatal("missing observed counter accepted")
	}
	if err := source.record(ctx, map[string]hysteria.Counter{"user-node": {TX: 1, RX: 1}}, at.Add(3*time.Second), false, ""); err == nil {
		t.Fatal("counter reset reused old source")
	}
	if err := source.recoverStopped(ctx, at.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	source, err = loadHysteriaSource(ctx, source.path, book)
	if err != nil {
		t.Fatal(err)
	}
	if !source.state.Closed {
		t.Fatal("orphan source not closed")
	}
	if err := source.recoverStopped(ctx, at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery, err := book.RecoveryRequest(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.ConfirmRecovery(ctx, recovery, protocol.TrafficRecoveryResponse{Version: 1, RequestID: recovery.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: recovery.LedgerEpoch, InstallationID: installation, Resume: true}); err != nil {
		t.Fatal(err)
	}
	report, err := book.Pending(ctx, authority, at.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Segments) != 1 {
		t.Fatalf("segments=%d", len(report.Segments))
	}
	segment := report.Segments[0]
	if segment.Totals.Uplink != 150 || segment.Totals.Downlink != 250 || !segment.Finalized || segment.Uncertainty == nil {
		t.Fatalf("bad final segment: %+v", segment)
	}
	diagnostic, err := book.Diagnostic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.Sources != 0 {
		t.Fatal("closed source watermarks leaked")
	}
	if len(diagnostic.PendingPreview) == 0 || diagnostic.PendingPreview[0].Totals.Uplink != 150 {
		t.Fatal("unacknowledged usage lost")
	}
	oldSource := source.state.SourceID
	if err := source.retire(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := newHysteriaSource(ctx, dir, book, node, policies, 2, at.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state.SourceID == oldSource {
		t.Fatal("restart reused counter identity")
	}
	if err := restarted.record(ctx, map[string]hysteria.Counter{"user-node": {TX: 10, RX: 20}}, at.Add(8*time.Second), false, ""); err != nil {
		t.Fatal(err)
	}
	diagnostic, err = book.Diagnostic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, segment := range diagnostic.PendingPreview {
		total += int64(segment.Totals.Uplink)
	}
	if total != 160 {
		t.Fatalf("restart total=%d, want 160", total)
	}
	if err := restarted.recoverStopped(ctx, at.Add(7*time.Second)); err != nil {
		t.Fatalf("clock rollback prevented source closure: %v", err)
	}
}
