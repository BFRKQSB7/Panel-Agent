// Deterministic executable contract exercised on Windows and the actual Alpine VPS.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

func id(n int) string       { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func at(s string) time.Time { t, err := time.Parse(time.RFC3339, s); must(err); return t }
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func check(ok bool) {
	if !ok {
		panic("contract mismatch")
	}
}
func main() {
	user, node, machine, epoch := id(1), id(2), id(3), id(4)
	scopes := []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: node}, {Kind: "machine", MachineID: machine}}
	policies := []period.Policy{}
	for i, day := range []int{1, 15, 31} {
		p := period.Policy{Scope: scopes[i], ID: id(10 + i), Revision: 1, EffectiveAt: at("2024-01-01T00:00:00Z"), Monthly: period.DefaultMonthly(day)}
		must(p.Validate())
		policies = append(policies, p)
	}
	slices, err := period.Split(policies, at("2024-02-01T00:00:00Z"), at("2024-03-02T00:00:00Z"))
	must(err)
	want := []string{"2024-02-15T00:00:00Z", "2024-02-29T00:00:00Z", "2024-03-01T00:00:00Z", "2024-03-02T00:00:00Z"}
	check(len(slices) == len(want))
	for i, s := range slices {
		check(s.End.Equal(at(want[i])))
	}
	gap, err := period.ResolveCivil("America/New_York", 2024, 3, 10, 2, 30, 0)
	must(err)
	check(gap.Equal(at("2024-03-10T07:00:00Z")))
	fold, err := period.ResolveCivil("America/New_York", 2024, 11, 3, 1, 30, 0)
	must(err)
	check(fold.Equal(at("2024-11-03T05:30:00Z")))
	newPolicy := policies[0]
	newPolicy.ID = id(20)
	newPolicy.EffectiveAt = at("2024-02-10T12:00:00Z")
	must(period.CheckPublication(policies, newPolicy))
	oldPeriod, err := period.At(policies[0], at("2024-02-15T00:00:00Z"))
	must(err)
	newPeriod, err := period.At(newPolicy, at("2024-02-15T00:00:00Z"))
	must(err)
	later, err := period.At(newPolicy, at("2024-02-20T00:00:00Z"))
	must(err)
	check(newPeriod.ID == later.ID && oldPeriod.ID != newPeriod.ID && newPeriod.Start.Equal(newPolicy.EffectiveAt))
	clamped, high, rewind, err := period.Resolve(newPolicy, at("2024-02-20T00:00:00Z"), at("2024-03-01T00:00:00Z"))
	must(err)
	check(rewind && clamped.Start.Equal(high))
	active := protocol.TrafficSegment{Key: protocol.SegmentKey{MachineID: machine, LedgerEpoch: epoch, StreamID: id(5), SegmentID: id(6)}, Stream: protocol.TrafficStream{Kind: "user_entry", UserID: user, EntryNodeID: node}, Periods: slices[0].Periods, Start: slices[0].Start, Through: slices[0].Start.Add(time.Minute), Totals: protocol.Totals{Uplink: quantity.Bytes(9007199254740993), Downlink: 7}}
	must(active.Validate())
	frozen := active
	frozen.Finalized = true
	frozen.CloseSeq = 1
	must(protocol.ValidateCumulativeUpdate(active, frozen))
	req := protocol.TrafficRequest{Version: 1, RequestID: id(7), AuthorityEpoch: id(8), MachineID: machine, LedgerEpoch: epoch, Segments: []protocol.TrafficSegment{frozen}}
	prefix, err := protocol.AdvanceClosedThrough(0, []uint64{3, 1})
	must(err)
	check(prefix == 1)
	receipt, err := security.SignClosedReceipt(bytes.Repeat([]byte{23}, 32), security.ClosedClaims{Version: 1, InstallationID: id(9), MachineID: machine, LedgerEpoch: epoch, ClosedThrough: prefix})
	must(err)
	must(security.VerifyClosedReceipt(bytes.Repeat([]byte{23}, 32), receipt, id(9), machine, epoch))
	ack := protocol.TrafficACK{Version: 1, RequestID: req.RequestID, AuthorityEpoch: req.AuthorityEpoch, MachineID: machine, LedgerEpoch: epoch, Receipt: &receipt, Outcomes: []protocol.TrafficOutcome{{Row: 0, Key: frozen.Key, Status: "final", Committed: &frozen.Totals, CloseSeq: 1}}}
	must(ack.ValidateFor(req, id(9)))
	retired := ack
	retired.Outcomes = []protocol.TrafficOutcome{{Row: 0, Key: frozen.Key, Status: "retired", CloseSeq: 1}}
	must(retired.ValidateFor(req, id(9)))
	result := struct {
		TZData  string                  `json:"tzdata"`
		Slices  []period.Slice          `json:"three_reset_days"`
		Gap     time.Time               `json:"dst_gap"`
		Fold    time.Time               `json:"dst_fold"`
		Old     period.Period           `json:"offline_old_period"`
		New     period.Period           `json:"synced_global_period"`
		Clamped period.Period           `json:"clock_rollback_period"`
		Active  protocol.TrafficSegment `json:"active"`
		Request protocol.TrafficRequest `json:"request"`
		ACK     protocol.TrafficACK     `json:"final_ack"`
		Retired protocol.TrafficACK     `json:"retired_ack"`
	}{period.TZDataID, slices, gap, fold, oldPeriod, newPeriod, clamped, active, req, ack, retired}
	must(json.NewEncoder(os.Stdout).Encode(result))
}
