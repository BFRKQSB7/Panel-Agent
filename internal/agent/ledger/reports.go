package ledger

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func (s *Store) PauseACKs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyAuthority = ""
	s.pending = nil
	s.recovery = nil
}

func (s *Store) RecoveryRequest(ctx context.Context, authority string) (protocol.TrafficRecoveryRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyAuthority = ""
	s.pending = nil
	s.recovery = nil
	r := protocol.TrafficRecoveryRequest{}
	if !security.IsUUIDv4(authority) {
		return r, ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return r, err
	}
	id, err := security.NewUUID()
	if err != nil {
		return r, err
	}
	r = protocol.TrafficRecoveryRequest{Version: 1, RequestID: id, AuthorityEpoch: authority, MachineID: m.MachineID, LedgerEpoch: m.Epoch, InstallationID: m.InstallationID, Allocated: m.Allocated, Receipt: m.Receipt, Held: []protocol.CloseRange{}}
	rows, err := tx.Query("SELECT close_seq FROM segments WHERE close_seq IS NOT NULL ORDER BY close_seq")
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var n uint64
		if err = rows.Scan(&n); err != nil {
			return r, err
		}
		last := len(r.Held) - 1
		if last >= 0 && r.Held[last].Last+1 == n {
			r.Held[last].Last = n
		} else {
			r.Held = append(r.Held, protocol.CloseRange{First: n, Last: n})
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	var copy protocol.TrafficRecoveryRequest
	if err = json.Unmarshal(encoded, &copy); err != nil {
		return r, err
	}
	s.recovery = &copy
	return r, nil
}

// A receipt may authorize removal only up to a prefix of locally allocated AND
// durably sent final records. Receiving a prefix never allocates sequence numbers.
func adoptReceipt(tx *sql.Tx, m *Metadata, r *security.ClosedReceipt) error {
	if r == nil {
		if m.Receipt != nil {
			return ErrLedger
		}
		return nil
	}
	mac, err := base64.RawURLEncoding.DecodeString(r.MAC)
	if r.ClosedClaims.Validate() != nil || err != nil || len(mac) != 32 || r.InstallationID != m.InstallationID || r.MachineID != m.MachineID || r.LedgerEpoch != m.Epoch || r.ClosedThrough > m.Allocated {
		return ErrLedger
	}
	previous := uint64(0)
	if m.Receipt != nil {
		previous = m.Receipt.ClosedThrough
		if r.ClosedThrough < previous {
			return ErrLedger
		}
	}
	var held, sent uint64
	if err = tx.QueryRow("SELECT COUNT(*),COALESCE(SUM(sent_final),0) FROM segments WHERE close_seq>? AND close_seq<=?", previous, r.ClosedThrough).Scan(&held, &sent); err != nil {
		return err
	}
	if held != r.ClosedThrough-previous || sent != held {
		return ErrLedger
	}
	copy := *r
	m.Receipt = &copy
	if _, err = tx.Exec("DELETE FROM segments WHERE close_seq<=?", r.ClosedThrough); err != nil {
		return err
	}
	return nil
}

func (s *Store) ConfirmRecovery(ctx context.Context, request protocol.TrafficRecoveryRequest, response protocol.TrafficRecoveryResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyAuthority = ""
	s.pending = nil
	if s.recovery == nil {
		return ErrLedger
	}
	a, _ := json.Marshal(s.recovery)
	b, _ := json.Marshal(request)
	if string(a) != string(b) {
		return ErrLedger
	}
	if !response.Resume || response.Version != 1 || response.RequestID != request.RequestID || !security.IsUUIDv4(request.RequestID) || response.AuthorityEpoch != request.AuthorityEpoch || response.MachineID != request.MachineID || response.LedgerEpoch != request.LedgerEpoch || !security.IsUUIDv4(response.InstallationID) {
		return ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return err
	}
	if m.MachineID != request.MachineID || m.Epoch != request.LedgerEpoch || m.InstallationID != "" && m.InstallationID != response.InstallationID {
		return ErrLedger
	}
	m.InstallationID = response.InstallationID
	m.Authority = response.AuthorityEpoch
	if err = adoptReceipt(tx, &m, response.Receipt); err != nil {
		return err
	}
	if err = writeMeta(tx, m); err != nil {
		return err
	}
	if err = s.commit(tx); err != nil {
		return err
	}
	s.readyAuthority = response.AuthorityEpoch
	s.recovery = nil
	return nil
}

// Pending snapshots are persisted before being returned to the transport. A
// retry uses unchanged K and totals from durable state, with a fresh request ID.
func (s *Store) Pending(ctx context.Context, authority string, now time.Time) (protocol.TrafficRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := protocol.TrafficRequest{}
	if s.readyAuthority == "" || s.readyAuthority != authority {
		return r, ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return r, err
	}
	id, err := security.NewUUID()
	if err != nil {
		return r, err
	}
	r = protocol.TrafficRequest{Version: 1, RequestID: id, AuthorityEpoch: authority, MachineID: m.MachineID, LedgerEpoch: m.Epoch, PreviousReceipt: m.Receipt, Segments: []protocol.TrafficSegment{}}
	rows, err := tx.Query("SELECT body FROM segments WHERE retry_after<=? ORDER BY CASE WHEN close_seq IS NULL THEN 1 ELSE 0 END,close_seq,id LIMIT ?", textTime(now), protocol.MaxTrafficBatch)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var b string
		var seg protocol.TrafficSegment
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return r, err
		}
		if err = json.Unmarshal([]byte(b), &seg); err != nil {
			rows.Close()
			return r, err
		}
		r.Segments = append(r.Segments, seg)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, err
	}
	for _, seg := range r.Segments {
		if seg.Finalized {
			if _, err = tx.Exec("UPDATE segments SET sent_final=1 WHERE id=?", seg.Key.SegmentID); err != nil {
				return r, err
			}
		}
	}
	if err = s.commit(tx); err != nil {
		return r, err
	}
	copyBytes, _ := json.Marshal(r)
	var copy protocol.TrafficRequest
	json.Unmarshal(copyBytes, &copy)
	s.pending = &copy
	return r, nil
}

func (s *Store) ConsumeACK(ctx context.Context, request protocol.TrafficRequest, ack protocol.TrafficACK, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.readyAuthority == "" || s.readyAuthority != ack.AuthorityEpoch {
		return ErrLedger
	}
	a, _ := json.Marshal(s.pending)
	b, _ := json.Marshal(request)
	if string(a) != string(b) {
		return ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return err
	}
	if err = ack.ValidateFor(request, m.InstallationID); err != nil {
		return err
	}
	for _, outcome := range ack.Outcomes {
		seg, err := readSegment(tx, outcome.Key.SegmentID)
		if err != nil {
			return err
		}
		var ackUp, ackDown int64
		var ackFinal bool
		if err = tx.QueryRow("SELECT ack_up,ack_down,ack_final FROM segments WHERE id=?", seg.Key.SegmentID).Scan(&ackUp, &ackDown, &ackFinal); err != nil {
			return err
		}
		if outcome.Status == "rejected" {
			code := outcome.ErrorCode
			if len(code) > 128 {
				code = code[:128]
			}
			if _, err = tx.Exec("UPDATE segments SET fault=?,retry_after=? WHERE id=?", code, textTime(now.Add(time.Minute)), seg.Key.SegmentID); err != nil {
				return err
			}
			continue
		}
		if outcome.Status == "retired" {
			if !seg.Finalized || seg.CloseSeq != outcome.CloseSeq {
				return ErrLedger
			}
			continue
		}
		totals := outcome.Committed
		if totals == nil || int64(totals.Uplink) < ackUp || int64(totals.Downlink) < ackDown || totals.Uplink > seg.Totals.Uplink || totals.Downlink > seg.Totals.Downlink || ackFinal && outcome.Status != "final" {
			return ErrLedger
		}
		if _, err = tx.Exec("UPDATE segments SET ack_up=?,ack_down=?,ack_final=?,fault='',retry_after='' WHERE id=?", int64(totals.Uplink), int64(totals.Downlink), outcome.Status == "final", seg.Key.SegmentID); err != nil {
			return err
		}
	}
	if ack.Receipt != nil {
		if err = adoptReceipt(tx, &m, ack.Receipt); err != nil {
			return err
		}
	}
	if err = writeMeta(tx, m); err != nil {
		return err
	}
	if err = s.commit(tx); err != nil {
		return err
	}
	s.pending = nil
	return nil
}
