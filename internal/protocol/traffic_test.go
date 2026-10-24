package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

const tid1 = "11111111-1111-4111-8111-111111111111"
const tid2 = "22222222-2222-4222-8222-222222222222"
const tid3 = "33333333-3333-4333-8333-333333333333"
const tid4 = "44444444-4444-4444-8444-444444444444"

func testSegment(t *testing.T, transit bool) TrafficSegment {
	t.Helper()
	start := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	s := TrafficSegment{Key: SegmentKey{tid1, tid2, tid3, tid4}, Stream: TrafficStream{Kind: "machine_only_transit"}, Start: start, Through: start.Add(time.Minute), Totals: Totals{quantity.Bytes(9007199254740993), 10}}
	scopes := []period.Scope{{Kind: "machine", MachineID: tid1}}
	if !transit {
		s.Stream = TrafficStream{"user_entry", tid2, tid3}
		scopes = append(scopes, period.Scope{Kind: "user", UserID: tid2}, period.Scope{Kind: "user_node", UserID: tid2, NodeID: tid3})
	}
	for _, scope := range scopes {
		policy, err := period.New(scope, 1, start, period.DefaultMonthly(1))
		if err != nil {
			t.Fatal(err)
		}
		p, err := period.At(policy, start)
		if err != nil {
			t.Fatal(err)
		}
		s.Periods = append(s.Periods, p)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestImmutableBindingFinalizationAndTransit(t *testing.T) {
	s := testSegment(t, false)
	next := s
	next.Totals.Uplink++
	next.Finalized = true
	next.CloseSeq = 7
	next.Through = next.Through.Add(time.Second)
	if ValidateCumulativeUpdate(s, next) != nil || ValidateCumulativeUpdate(next, next) != nil {
		t.Fatal("valid cumulative/final replay rejected")
	}
	bad := next
	bad.Totals.Downlink++
	if ValidateCumulativeUpdate(next, bad) == nil {
		t.Fatal("final totals grew")
	}
	bad = next
	bad.CloseSeq++
	if ValidateCumulativeUpdate(next, bad) == nil {
		t.Fatal("final sequence changed")
	}
	bad = s
	bad.Finalized = false
	bad.CloseSeq = 1
	if bad.Validate() == nil {
		t.Fatal("sequence allocated before freeze")
	}
	bad = s
	bad.Totals = Totals{math.MaxInt64, 1}
	if bad.Validate() == nil {
		t.Fatal("sum overflow")
	}
	bad = s
	bad.Periods = append([]period.Period(nil), s.Periods...)
	replacement, _ := period.New(s.Periods[1].Scope, 1, s.Start, period.DefaultMonthly(1))
	bad.Periods[1], _ = period.At(replacement, s.Start)
	if bad.Validate() != nil || ValidateCumulativeUpdate(s, bad) == nil {
		t.Fatal("same K reassigned policy")
	}
	transit := testSegment(t, true)
	transit.Stream.UserID = tid2
	if transit.Validate() == nil {
		t.Fatal("transit attributed to user")
	}
	transit.Stream.UserID = ""
	transit.Periods = append(transit.Periods, s.Periods[1])
	if transit.Validate() == nil {
		t.Fatal("transit has fabricated user period")
	}
}

func TestACKKindsAttributionPrefixAndRowIsolation(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"uplink":"0"}`, `{"uplink":null,"downlink":"0"}`, `{"uplink":"0","downlink":"0","other":"0"}`} {
		var totals Totals
		if json.Unmarshal([]byte(raw), &totals) == nil {
			t.Fatalf("missing or malformed totals accepted: %s", raw)
		}
	}
	s := testSegment(t, true)
	r := TrafficRequest{Version: 1, RequestID: tid4, AuthorityEpoch: tid3, MachineID: tid1, LedgerEpoch: tid2, Segments: []TrafficSegment{s}}
	a := TrafficACK{Version: 1, RequestID: tid4, AuthorityEpoch: tid3, MachineID: tid1, LedgerEpoch: tid2, Outcomes: []TrafficOutcome{{Row: 0, Key: s.Key, Status: "cumulative", Committed: &s.Totals}}}
	if a.ValidateFor(r, tid4) != nil {
		t.Fatal("ordinary ACK rejected")
	}
	a.AuthorityEpoch = tid4
	if a.ValidateFor(r, tid4) == nil {
		t.Fatal("wrong authority accepted")
	}
	a.AuthorityEpoch = tid3
	s.Finalized = true
	s.CloseSeq = 3
	r.Segments[0] = s
	a.Outcomes[0].Status = "final"
	a.Outcomes[0].CloseSeq = 3
	if a.ValidateFor(r, tid4) != nil {
		t.Fatal("final ACK rejected")
	}
	through, err := AdvanceClosedThrough(0, []uint64{1, 3})
	if err != nil || through != 1 {
		t.Fatal("gap skipped")
	}
	through, err = AdvanceClosedThrough(through, []uint64{3, 2})
	if err != nil || through != 3 {
		t.Fatal("contiguous prefix not advanced")
	}
	receipt, _ := security.SignClosedReceipt(bytes.Repeat([]byte{7}, 32), security.ClosedClaims{Version: 1, InstallationID: tid4, MachineID: tid1, LedgerEpoch: tid2, ClosedThrough: through})
	a.Receipt = &receipt
	a.Outcomes[0].Status = "retired"
	a.Outcomes[0].Committed = nil
	if a.ValidateFor(r, tid4) != nil {
		t.Fatal("retirement proof rejected")
	}
	a.Receipt.ClosedThrough = 2
	if a.ValidateFor(r, tid4) == nil {
		t.Fatal("uncommitted gap retired")
	}
	good, _ := json.Marshal(s)
	bad := bytes.Replace(good, []byte(`"9007199254740993"`), []byte(`"1e3"`), 1)
	w := TrafficWireRequest{Version: 1, RequestID: tid4, AuthorityEpoch: tid3, MachineID: tid1, LedgerEpoch: tid2, Segments: []json.RawMessage{bad, good}}
	_, rows, err := w.DecodeRows()
	if err != nil || len(rows) != 2 || rows[0].Err == nil || rows[1].Err != nil {
		t.Fatal("bad row blocked good report", rows, err)
	}
}
