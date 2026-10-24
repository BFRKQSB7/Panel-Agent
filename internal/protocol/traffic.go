package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

const TrafficVersion = 1
const MaxTrafficBatch = 256

// Recovery always precedes traffic ACK consumption after process start or an
// authority change. Held ranges describe local final records, not new charges.
type CloseRange struct {
	First uint64 `json:"first,string"`
	Last  uint64 `json:"last,string"`
}
type TrafficRecoveryRequest struct {
	Version        int                     `json:"version"`
	RequestID      string                  `json:"request_id"`
	AuthorityEpoch string                  `json:"authority_epoch"`
	MachineID      string                  `json:"machine_id"`
	LedgerEpoch    string                  `json:"ledger_epoch"`
	InstallationID string                  `json:"installation_id,omitempty"`
	Allocated      uint64                  `json:"allocated_close_seq,string"`
	Held           []CloseRange            `json:"held_final_ranges"`
	Receipt        *security.ClosedReceipt `json:"receipt,omitempty"`
}
type TrafficRecoveryResponse struct {
	Version        int                     `json:"version"`
	RequestID      string                  `json:"request_id"`
	AuthorityEpoch string                  `json:"authority_epoch"`
	MachineID      string                  `json:"machine_id"`
	LedgerEpoch    string                  `json:"ledger_epoch"`
	InstallationID string                  `json:"installation_id"`
	Resume         bool                    `json:"resume"`
	Receipt        *security.ClosedReceipt `json:"receipt,omitempty"`
}

// ValidateEnvelope checks only the recovery header and the locally held final
// sequence ranges. The Panel still authenticates the machine and verifies the
// receipt MAC before changing any persisted state.
func (r TrafficRecoveryRequest) ValidateEnvelope() error {
	if r.Version != TrafficVersion || !security.IsUUIDv4(r.RequestID) ||
		!security.IsUUIDv4(r.AuthorityEpoch) || !security.IsUUIDv4(r.MachineID) ||
		!security.IsUUIDv4(r.LedgerEpoch) || r.Allocated > 1<<63-1 {
		return ErrTraffic
	}
	if r.InstallationID != "" && !security.IsUUIDv4(r.InstallationID) {
		return ErrTraffic
	}
	if r.Receipt != nil {
		if r.Receipt.ClosedClaims.Validate() != nil ||
			r.Receipt.MachineID != r.MachineID ||
			r.Receipt.LedgerEpoch != r.LedgerEpoch ||
			r.Receipt.ClosedThrough > r.Allocated {
			return ErrTraffic
		}
	}
	var previous uint64
	for i, held := range r.Held {
		if held.First == 0 || held.Last < held.First || held.Last > r.Allocated ||
			(i > 0 && held.First <= previous) {
			return ErrTraffic
		}
		previous = held.Last
	}
	return nil
}

var ErrTraffic = errors.New("无效或冲突的流量报告")

type SegmentKey struct {
	MachineID   string `json:"machine_id"`
	LedgerEpoch string `json:"ledger_epoch"`
	StreamID    string `json:"stream_id"`
	SegmentID   string `json:"segment_id"`
}

func (k SegmentKey) Validate() error {
	if !security.IsUUIDv4(k.MachineID) || !security.IsUUIDv4(k.LedgerEpoch) || !security.IsUUIDv4(k.StreamID) || !security.IsUUIDv4(k.SegmentID) {
		return ErrTraffic
	}
	return nil
}

type TrafficStream struct {
	Kind        string `json:"kind"`
	UserID      string `json:"user_id,omitempty"`
	EntryNodeID string `json:"entry_node_id,omitempty"`
}
type Totals struct {
	Uplink   quantity.Bytes `json:"uplink"`
	Downlink quantity.Bytes `json:"downlink"`
}

func (t *Totals) UnmarshalJSON(data []byte) error {
	var raw struct {
		Uplink   json.RawMessage `json:"uplink"`
		Downlink json.RawMessage `json:"downlink"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil {
		return err
	}
	var next Totals
	if err := json.Unmarshal(raw.Uplink, &next.Uplink); err != nil {
		return err
	}
	if err := json.Unmarshal(raw.Downlink, &next.Downlink); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*t = next
	return nil
}

func (t Totals) Validate() error { _, err := quantity.Add(t.Uplink, t.Downlink); return err }

// Frozen totals, Through and CloseSeq can never change. close_seq is allocated
// by D08 in the freeze transaction, monotonically per machine x ledger_epoch.
// Core generations are observation provenance, never part of this identity.
type TrafficSegment struct {
	Key         SegmentKey         `json:"key"`
	Stream      TrafficStream      `json:"stream"`
	Periods     []period.Period    `json:"periods"`
	Start       time.Time          `json:"start_utc"`
	Through     time.Time          `json:"through_utc"`
	Totals      Totals             `json:"totals"`
	Finalized   bool               `json:"finalized"`
	CloseSeq    uint64             `json:"close_seq,string"`
	Uncertainty *ObservationWindow `json:"uncertainty,omitempty"`
}

// A conservative envelope of ambiguous observations, not an invented error
// bound. The true byte distribution inside this interval is unknown.
type ObservationWindow struct {
	From      time.Time `json:"from_utc"`
	To        time.Time `json:"to_utc"`
	Reason    string    `json:"reason"`
	Unbounded bool      `json:"no_error_bound"`
}

func (s TrafficSegment) Validate() error {
	if u := s.Uncertainty; u != nil && (!period.ValidUTC(u.From) || !period.ValidUTC(u.To) || u.To.Before(u.From) || u.Reason == "") {
		return ErrTraffic
	}
	if s.Key.Validate() != nil || s.Totals.Validate() != nil || !period.ValidUTC(s.Start) || !period.ValidUTC(s.Through) || s.Through.Before(s.Start) || s.Finalized != (s.CloseSeq > 0) || s.CloseSeq > 1<<63-1 {
		return ErrTraffic
	}
	expected := map[period.Scope]bool{{Kind: "machine", MachineID: s.Key.MachineID}: true}
	switch s.Stream.Kind {
	case "user_entry":
		if !security.IsUUIDv4(s.Stream.UserID) || !security.IsUUIDv4(s.Stream.EntryNodeID) {
			return ErrTraffic
		}
		expected[period.Scope{Kind: "user", UserID: s.Stream.UserID}] = true
		expected[period.Scope{Kind: "user_node", UserID: s.Stream.UserID, NodeID: s.Stream.EntryNodeID}] = true
	case "machine_only_transit":
		if s.Stream.UserID != "" || s.Stream.EntryNodeID != "" {
			return ErrTraffic
		}
	default:
		return ErrTraffic
	}
	if len(s.Periods) != len(expected) {
		return ErrTraffic
	}
	for _, p := range s.Periods {
		if p.Validate() != nil || !expected[p.Scope] || s.Start.Before(p.Start) || !s.Start.Before(p.End) || s.Through.After(p.End) {
			return ErrTraffic
		}
		delete(expected, p.Scope)
	}
	return nil
}

func (s TrafficSegment) sameBinding(next TrafficSegment) bool {
	if s.Key != next.Key || s.Stream != next.Stream || !s.Start.Equal(next.Start) || len(s.Periods) != len(next.Periods) {
		return false
	}
	ps := map[string]bool{}
	for _, p := range s.Periods {
		ps[p.ID] = true
	}
	for _, p := range next.Periods {
		if !ps[p.ID] {
			return false
		}
	}
	return true
}
func ValidateCumulativeUpdate(previous, next TrafficSegment) error {
	if previous.Validate() != nil || next.Validate() != nil || !previous.sameBinding(next) || next.Through.Before(previous.Through) || next.Totals.Uplink < previous.Totals.Uplink || next.Totals.Downlink < previous.Totals.Downlink {
		return ErrTraffic
	}
	if previous.Finalized && (!next.Finalized || next.CloseSeq != previous.CloseSeq || next.Totals != previous.Totals || !next.Through.Equal(previous.Through)) {
		return ErrTraffic
	}
	if previous.Finalized {
		a, _ := json.Marshal(previous.Uncertainty)
		b, _ := json.Marshal(next.Uncertainty)
		if !bytes.Equal(a, b) {
			return ErrTraffic
		}
	}
	return nil
}

type TrafficRequest struct {
	Version         int                     `json:"version"`
	RequestID       string                  `json:"request_id"`
	AuthorityEpoch  string                  `json:"authority_epoch"`
	MachineID       string                  `json:"machine_id"`
	LedgerEpoch     string                  `json:"ledger_epoch"`
	Segments        []TrafficSegment        `json:"segments"`
	PreviousReceipt *security.ClosedReceipt `json:"previous_receipt,omitempty"`
}

// ValidateEnvelope intentionally does not validate rows. Ingest must return a
// separate outcome for each attributable row; one bad row cannot block good rows.
func (r TrafficRequest) ValidateEnvelope() error {
	if r.Version != TrafficVersion || !security.IsUUIDv4(r.RequestID) || !security.IsUUIDv4(r.AuthorityEpoch) || !security.IsUUIDv4(r.MachineID) || !security.IsUUIDv4(r.LedgerEpoch) || len(r.Segments) == 0 || len(r.Segments) > MaxTrafficBatch {
		return ErrTraffic
	}
	if r.PreviousReceipt != nil && (r.PreviousReceipt.ClosedClaims.Validate() != nil || r.PreviousReceipt.MachineID != r.MachineID || r.PreviousReceipt.LedgerEpoch != r.LedgerEpoch) {
		return ErrTraffic
	}
	return nil
}

// DecodeTrafficRows preserves row-level failure isolation, including malformed
// amount strings. Header syntax/authentication failures still reject the batch.
// D09 must bound request bytes before decoding and authenticate the machine.
type TrafficWireRequest struct {
	Version         int                     `json:"version"`
	RequestID       string                  `json:"request_id"`
	AuthorityEpoch  string                  `json:"authority_epoch"`
	MachineID       string                  `json:"machine_id"`
	LedgerEpoch     string                  `json:"ledger_epoch"`
	Segments        []json.RawMessage       `json:"segments"`
	PreviousReceipt *security.ClosedReceipt `json:"previous_receipt,omitempty"`
}
type TrafficRow struct {
	Segment TrafficSegment
	Err     error
}

func (w TrafficWireRequest) DecodeRows() (TrafficRequest, []TrafficRow, error) {
	r := TrafficRequest{Version: w.Version, RequestID: w.RequestID, AuthorityEpoch: w.AuthorityEpoch, MachineID: w.MachineID, LedgerEpoch: w.LedgerEpoch, PreviousReceipt: w.PreviousReceipt, Segments: make([]TrafficSegment, len(w.Segments))}
	if err := r.ValidateEnvelope(); err != nil {
		return r, nil, err
	}
	rows := make([]TrafficRow, len(w.Segments))
	seen := map[SegmentKey]bool{}
	for i, raw := range w.Segments {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&r.Segments[i])
		s := r.Segments[i]
		if err == nil {
			err = s.Validate()
		}
		if err == nil && (s.Key.MachineID != r.MachineID || s.Key.LedgerEpoch != r.LedgerEpoch || seen[s.Key]) {
			err = ErrTraffic
		}
		if err == nil {
			seen[s.Key] = true
		}
		rows[i] = TrafficRow{s, err}
	}
	return r, rows, nil
}

type TrafficOutcome struct {
	Row       int        `json:"row"`
	Key       SegmentKey `json:"key"`
	Status    string     `json:"status"` // cumulative, final, retired, rejected
	Committed *Totals    `json:"committed,omitempty"`
	CloseSeq  uint64     `json:"close_seq,string"`
	ErrorCode string     `json:"error_code,omitempty"`
}
type TrafficACK struct {
	Version        int                     `json:"version"`
	RequestID      string                  `json:"request_id"`
	AuthorityEpoch string                  `json:"authority_epoch"`
	MachineID      string                  `json:"machine_id"`
	LedgerEpoch    string                  `json:"ledger_epoch"`
	Outcomes       []TrafficOutcome        `json:"outcomes"`
	Receipt        *security.ClosedReceipt `json:"receipt,omitempty"`
}

// ValidateFor checks acknowledgement attribution on the Agent. The receipt MAC
// is verified by the Panel, never with a secret shipped to Agents. Authenticated
// transport and persisted installation identity remain mandatory.
func (a TrafficACK) ValidateFor(r TrafficRequest, installation string) error {
	if r.ValidateEnvelope() != nil || a.Version != TrafficVersion || a.RequestID != r.RequestID || a.AuthorityEpoch != r.AuthorityEpoch || a.MachineID != r.MachineID || a.LedgerEpoch != r.LedgerEpoch || len(a.Outcomes) != len(r.Segments) {
		return ErrTraffic
	}
	if a.Receipt != nil && (a.Receipt.ClosedClaims.Validate() != nil || a.Receipt.InstallationID != installation || a.Receipt.MachineID != r.MachineID || a.Receipt.LedgerEpoch != r.LedgerEpoch || a.Receipt.MAC == "") {
		return ErrTraffic
	}
	seen := map[int]bool{}
	for _, o := range a.Outcomes {
		if o.Row < 0 || o.Row >= len(r.Segments) || seen[o.Row] {
			return ErrTraffic
		}
		seen[o.Row] = true
		s := r.Segments[o.Row]
		if o.Key != s.Key {
			return ErrTraffic
		}
		if o.Status == "rejected" {
			if o.Committed != nil || o.CloseSeq != 0 || o.ErrorCode == "" {
				return ErrTraffic
			}
			continue
		}
		if s.Validate() != nil || s.Key.MachineID != r.MachineID || s.Key.LedgerEpoch != r.LedgerEpoch || o.ErrorCode != "" {
			return ErrTraffic
		}
		switch o.Status {
		case "cumulative":
			if s.Finalized || o.CloseSeq != 0 || o.Committed == nil || o.Committed.Validate() != nil || o.Committed.Uplink < s.Totals.Uplink || o.Committed.Downlink < s.Totals.Downlink {
				return ErrTraffic
			}
		case "final":
			if !s.Finalized || o.CloseSeq != s.CloseSeq || o.Committed == nil || *o.Committed != s.Totals {
				return ErrTraffic
			}
		case "retired":
			if !s.Finalized || o.CloseSeq != s.CloseSeq || o.Committed != nil || a.Receipt == nil || s.CloseSeq > a.Receipt.ClosedThrough {
				return ErrTraffic
			}
		default:
			return ErrTraffic
		}
	}
	return nil
}

// AdvanceClosedThrough accepts only close_seq values already committed in the
// caller's transaction. A gap stops advancement; max(seen) is never a watermark.
func AdvanceClosedThrough(current uint64, committed []uint64) (uint64, error) {
	if current > 1<<63-1 {
		return 0, ErrTraffic
	}
	seen := map[uint64]bool{}
	for _, n := range committed {
		if n == 0 || n > 1<<63-1 {
			return 0, ErrTraffic
		}
		seen[n] = true
	}
	for current < 1<<63-1 && seen[current+1] {
		current++
	}
	return current, nil
}
