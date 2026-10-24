package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

const SampleInterval = 5 * time.Second

type Reading struct {
	Label  string
	Stream protocol.TrafficStream
	Totals protocol.Totals
}
type Sample struct {
	ImportID           string
	SourceID           string
	Generation         uint64
	Started            time.Time
	Finished           time.Time
	Policies           []period.Policy
	Readings           []Reading
	Finalize           bool
	Gap                string
	PolicyAppliedAt    time.Time
	HistoricalBoundary bool
}

func selectPeriods(policies []period.Policy, stream protocol.TrafficStream, machine string, at time.Time) ([]period.Period, error) {
	scopes := []period.Scope{{Kind: "machine", MachineID: machine}}
	if stream.Kind == "user_entry" {
		scopes = append(scopes, period.Scope{Kind: "user", UserID: stream.UserID}, period.Scope{Kind: "user_node", UserID: stream.UserID, NodeID: stream.EntryNodeID})
	} else if stream.Kind != "machine_only_transit" || stream.UserID != "" || stream.EntryNodeID != "" {
		return nil, ErrLedger
	}
	result := []period.Period{}
	for _, scope := range scopes {
		var chosen *period.Policy
		for i := range policies {
			p := &policies[i]
			if p.Scope == scope && !p.EffectiveAt.After(at) && (chosen == nil || p.EffectiveAt.After(chosen.EffectiveAt)) {
				chosen = p
			}
		}
		if chosen == nil {
			return nil, ErrLedger
		}
		p, err := period.At(*chosen, at)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Scope.Key() < result[j].Scope.Key() })
	return result, nil
}

func saveSegment(tx *sql.Tx, s protocol.TrafficSegment) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var seq any
	if s.Finalized {
		seq = s.CloseSeq
	}
	_, err = tx.Exec("INSERT INTO segments(id,stream_id,close_seq,body) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET close_seq=excluded.close_seq,body=excluded.body", s.Key.SegmentID, s.Key.StreamID, seq, string(b))
	return err
}
func readSegment(tx *sql.Tx, id string) (protocol.TrafficSegment, error) {
	var s protocol.TrafficSegment
	var b string
	err := tx.QueryRow("SELECT body FROM segments WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &s)
	}
	if err == nil {
		err = s.Validate()
	}
	return s, err
}
func freeze(tx *sql.Tx, meta *Metadata, s *protocol.TrafficSegment, at time.Time) error {
	if s.Finalized {
		return nil
	}
	if meta.Allocated >= 1<<63-1 {
		return ErrLedger
	}
	end := at
	for _, p := range s.Periods {
		if p.End.Before(end) {
			end = p.End
		}
	}
	if end.Before(s.Through) {
		end = s.Through
	}
	s.Through = end
	meta.Allocated++
	s.Finalized = true
	s.CloseSeq = meta.Allocated
	if err := saveSegment(tx, *s); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE streams SET active='' WHERE id=? AND active=?", s.Key.StreamID, s.Key.SegmentID)
	return err
}
func uncertain(s *protocol.TrafficSegment, from, to time.Time, reason string) {
	if to.Before(from) {
		from, to = to, from
	}
	if s.Uncertainty == nil {
		s.Uncertainty = &protocol.ObservationWindow{From: from, To: to, Reason: reason, Unbounded: true}
		return
	}
	u := s.Uncertainty
	if from.Before(u.From) {
		u.From = from
	}
	if to.After(u.To) {
		u.To = to
	}
	if u.Reason != reason {
		u.Reason = "multiple_observation_gaps"
	}
	u.Unbounded = true
}

// Ingest changes all raw watermarks, segment totals, close sequences and the
// accounting clock in the SAME transaction. Failure leaves all of them intact.
func (s *Store) Ingest(ctx context.Context, sample Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !security.IsUUIDv4(sample.SourceID) || sample.Generation == 0 || sample.Generation > 1<<63-1 || !period.ValidUTC(sample.Started) || !period.ValidUTC(sample.Finished) || len(sample.Readings) > 4096 || len(sample.Policies) > 4096 {
		return ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	meta, err := readMeta(tx)
	if err != nil {
		return err
	}
	if sample.ImportID != "" {
		var n int
		if err = tx.QueryRow("SELECT COUNT(*) FROM imports WHERE id=?", sample.ImportID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	for i, p := range sample.Policies {
		if err := period.CheckPublication(sample.Policies[:i], p); err != nil {
			return err
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRow("SELECT body FROM policies WHERE id=?", p.ID).Scan(&existing)
		if err == nil {
			if existing != string(b) {
				return ErrLedger
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.Exec("INSERT INTO policies(id,scope_key,effective_at,body) VALUES(?,?,?,?)", p.ID, p.Scope.Key(), textTime(p.EffectiveAt), string(b)); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	at := sample.Finished
	clockBack := at.Before(meta.HighWater)
	if clockBack {
		at = meta.HighWater
	}
	var generation uint64
	var known, closed bool
	var startedText string
	err = tx.QueryRow("SELECT generation,known_zero,started,closed FROM sources WHERE id=?", sample.SourceID).Scan(&generation, &known, &startedText, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		generation = sample.Generation
		startedText = textTime(sample.Started)
		if _, err = tx.Exec("INSERT INTO sources(id,generation,known_zero,started) VALUES(?,?,0,?)", sample.SourceID, generation, startedText); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if generation != sample.Generation || closed {
		return ErrLedger
	}
	started, err := parseTime(startedText)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, reading := range sample.Readings {
		if reading.Label == "" || len(reading.Label) > 128 || seen[reading.Label] || reading.Totals.Validate() != nil {
			return ErrLedger
		}
		seen[reading.Label] = true
		bindingBytes, _ := json.Marshal(reading.Stream)
		binding := string(bindingBytes)
		var streamID, activeID string
		err = tx.QueryRow("SELECT id,active FROM streams WHERE binding=?", binding).Scan(&streamID, &activeID)
		if errors.Is(err, sql.ErrNoRows) {
			streamID, err = security.NewUUID()
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO streams(id,binding) VALUES(?,?)", streamID, binding); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var last protocol.Totals
		var lastObserved, oldBinding, oldStream string
		err = tx.QueryRow("SELECT up,down,observed,binding,stream_id FROM watermarks WHERE source=? AND label=?", sample.SourceID, reading.Label).Scan(&last.Uplink, &last.Downlink, &lastObserved, &oldBinding, &oldStream)
		first := errors.Is(err, sql.ErrNoRows)
		if err != nil && !first {
			return err
		}
		if !first && (oldBinding != binding || oldStream != streamID) {
			return ErrLedger
		}
		from := started
		if !first {
			from, err = parseTime(lastObserved)
			if err != nil {
				return err
			}
		}
		if first && !known {
			last = reading.Totals
			if err = event(tx, "unproven_source_baseline", from, sample.Finished); err != nil {
				return err
			}
		}
		// A durable boundary file can precede a newer regular sample if its
		// journal update failed. It must not rewind or rebill that watermark.
		if sample.HistoricalBoundary && !first && !sample.Finished.After(from) && reading.Totals.Uplink <= last.Uplink && reading.Totals.Downlink <= last.Downlink {
			continue
		}
		if reading.Totals.Uplink < last.Uplink || reading.Totals.Downlink < last.Downlink {
			return ErrLedger
		}
		delta := protocol.Totals{Uplink: reading.Totals.Uplink - last.Uplink, Downlink: reading.Totals.Downlink - last.Downlink}
		periods, err := selectPeriods(sample.Policies, reading.Stream, meta.MachineID, at)
		if err != nil {
			return err
		}
		var segment protocol.TrafficSegment
		changed := false
		if activeID != "" {
			segment, err = readSegment(tx, activeID)
			if err != nil {
				return err
			}
			changed = len(segment.Periods) != len(periods)
			if !changed {
				for i := range periods {
					if segment.Periods[i].ID != periods[i].ID {
						changed = true
						break
					}
				}
			}
			if changed {
				if err = freeze(tx, &meta, &segment, at); err != nil {
					return err
				}
				activeID = ""
			}
		}
		if activeID == "" {
			id, err := security.NewUUID()
			if err != nil {
				return err
			}
			start := from
			if sample.PolicyAppliedAt.After(start) {
				start = sample.PolicyAppliedAt
			}
			if start.After(at) {
				start = at
			}
			for _, p := range periods {
				if p.Start.After(start) {
					start = p.Start
				}
			}
			segment = protocol.TrafficSegment{Key: protocol.SegmentKey{MachineID: meta.MachineID, LedgerEpoch: meta.Epoch, StreamID: streamID, SegmentID: id}, Stream: reading.Stream, Periods: periods, Start: start, Through: at}
			if _, err = tx.Exec("UPDATE streams SET active=? WHERE id=?", id, streamID); err != nil {
				return err
			}
		}
		segment.Through = at
		segment.Totals.Uplink, err = quantity.Add(segment.Totals.Uplink, delta.Uplink)
		if err != nil {
			return err
		}
		segment.Totals.Downlink, err = quantity.Add(segment.Totals.Downlink, delta.Downlink)
		if err != nil {
			return err
		}
		crossed := changed
		for _, p := range periods {
			crossed = crossed || from.Before(p.Start)
		}
		if crossed {
			uncertain(&segment, from, sample.Finished, "missed_period_or_policy_boundary")
		}
		if !sample.PolicyAppliedAt.IsZero() && from.Before(sample.PolicyAppliedAt) && (delta.Uplink > 0 || delta.Downlink > 0) {
			uncertain(&segment, from, sample.Finished, "policy_application_sampling_interval")
		}
		if clockBack || sample.Finished.Before(sample.Started) {
			uncertain(&segment, from, sample.Finished, "clock_rollback")
		}
		if sample.Finished.Sub(from) > 2*SampleInterval {
			uncertain(&segment, from, sample.Finished, "sampling_gap")
		}
		if sample.Gap != "" {
			uncertain(&segment, from, sample.Finished, sample.Gap)
		}
		if err = saveSegment(tx, segment); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO watermarks(source,label,stream_id,up,down,observed,binding) VALUES(?,?,?,?,?,?,?) ON CONFLICT(source,label) DO UPDATE SET up=excluded.up,down=excluded.down,observed=excluded.observed", sample.SourceID, reading.Label, streamID, int64(reading.Totals.Uplink), int64(reading.Totals.Downlink), textTime(sample.Finished), binding); err != nil {
			return err
		}
	}
	if sample.Finalize {
		rows, err := tx.Query("SELECT DISTINCT streams.active FROM streams JOIN watermarks ON watermarks.stream_id=streams.id WHERE watermarks.source=? AND streams.active!='' ORDER BY streams.id", sample.SourceID)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			seg, err := readSegment(tx, id)
			if err != nil {
				return err
			}
			if sample.Gap != "" {
				uncertain(&seg, seg.Through, sample.Finished, sample.Gap)
			}
			if err = freeze(tx, &meta, &seg, at); err != nil {
				return err
			}
		}
	}
	if sample.Gap != "" {
		if err = event(tx, sample.Gap, sample.Started, sample.Finished); err != nil {
			return err
		}
	}
	meta.HighWater = at
	if err = writeMeta(tx, meta); err != nil {
		return err
	}
	if sample.ImportID != "" {
		if _, err = tx.Exec("INSERT INTO imports(id) VALUES(?)", sample.ImportID); err != nil {
			return err
		}
	}
	return s.commit(tx)
}

func (s *Store) RecordGap(ctx context.Context, code string, from, to time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = event(tx, code, from, to); err != nil {
		return err
	}
	return s.commit(tx)
}

// ForgetImport follows durable unlink of the consumed boundary file. Old source
// watermarks are removed only after its process is stopped and its final sample
// committed; final segments remain until the signed contiguous prefix permits GC.
func (s *Store) ForgetImport(ctx context.Context, id, source string, stopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM imports WHERE id=?", id); err != nil {
		return err
	}
	if stopped {
		if _, err = tx.Exec("DELETE FROM watermarks WHERE source=?", source); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM sources WHERE id=?", source); err != nil {
			return err
		}
	}
	return s.commit(tx)
}

func (s *Store) NextBoundary(ctx context.Context) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, "SELECT body FROM segments WHERE close_seq IS NULL")
	if err != nil {
		return time.Time{}
	}
	defer rows.Close()
	var next time.Time
	for rows.Next() {
		var b string
		var seg protocol.TrafficSegment
		if rows.Scan(&b) != nil || json.Unmarshal([]byte(b), &seg) != nil {
			return time.Time{}
		}
		for _, p := range seg.Periods {
			if next.IsZero() || p.End.Before(next) {
				next = p.End
			}
		}
	}
	return next
}
