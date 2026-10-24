package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrTrafficInvalid     = errors.New("invalid traffic state")
	ErrTrafficEpoch       = errors.New("unknown or conflicting traffic ledger epoch")
	ErrTrafficReceipt     = errors.New("invalid traffic closed-prefix receipt")
	ErrTrafficRecoveryGap = errors.New("traffic recovery gap")
	ErrTrafficPolicy      = errors.New("traffic segment references an unknown policy")
)

type trafficLedger struct {
	MachineID      string
	LedgerEpoch    string
	InstallationID string
	Allocated      uint64
	ClosedThrough  uint64
}

type trafficRowReject struct{ code string }

func (e *trafficRowReject) Error() string { return e.code }

const (
	trafficErrorInvalid          = "invalid_row"
	trafficErrorUnknownPolicy    = "unknown_policy"
	trafficErrorSegmentConflict  = "segment_conflict"
	trafficErrorCloseSeqConflict = "close_seq_conflict"
	trafficErrorClosedConflict   = "closed_segment_conflict"
)

func trafficNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func trafficTextTime(now time.Time) string { return trafficNow(now).Format(time.RFC3339Nano) }

func (s *Store) panelInstallationTx(ctx context.Context, tx *sql.Tx) (string, error) {
	var installation string
	if err := tx.QueryRowContext(ctx, "SELECT installation_id FROM installation WHERE id=1").Scan(&installation); err != nil {
		return "", fmt.Errorf("read Panel installation identity: %w", err)
	}
	if !security.IsUUIDv4(installation) {
		return "", ErrTrafficInvalid
	}
	return installation, nil
}

func readTrafficLedger(ctx context.Context, tx *sql.Tx, machineID string) (trafficLedger, error) {
	var ledger trafficLedger
	var allocated, closed int64
	err := tx.QueryRowContext(ctx, `SELECT machine_id,ledger_epoch,installation_id,allocated_close_seq,closed_through
		FROM traffic_ledgers WHERE machine_id=?`, machineID).Scan(
		&ledger.MachineID, &ledger.LedgerEpoch, &ledger.InstallationID, &allocated, &closed)
	if err != nil {
		return ledger, err
	}
	if allocated < 0 || closed < 0 || closed > allocated ||
		!security.IsUUIDv4(ledger.MachineID) || !security.IsUUIDv4(ledger.LedgerEpoch) ||
		!security.IsUUIDv4(ledger.InstallationID) {
		return trafficLedger{}, ErrTrafficInvalid
	}
	ledger.Allocated, ledger.ClosedThrough = uint64(allocated), uint64(closed)
	return ledger, nil
}

func writeTrafficLedger(ctx context.Context, tx *sql.Tx, ledger trafficLedger, now time.Time) error {
	if ledger.Allocated > 1<<63-1 || ledger.ClosedThrough > ledger.Allocated {
		return ErrTrafficInvalid
	}
	_, err := tx.ExecContext(ctx, `UPDATE traffic_ledgers
		SET allocated_close_seq=?,closed_through=?,updated_at=? WHERE machine_id=?`,
		int64(ledger.Allocated), int64(ledger.ClosedThrough), trafficTextTime(now), ledger.MachineID)
	return err
}

func validateTrafficAuthority(ctx context.Context, tx *sql.Tx, machineID, authority string) error {
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return err
	}
	if machine.Revoked {
		return ErrMachineAuthentication
	}
	current, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return err
	}
	if !security.IsUUIDv4(authority) || machine.AuthorityEpoch != authority || current.Epoch != authority {
		return ErrAuthorityMismatch
	}
	return nil
}

func validateTrafficReceipt(master []byte, receipt *security.ClosedReceipt, installation, machine, epoch string, max uint64) error {
	if receipt == nil {
		return nil
	}
	if receipt.ClosedThrough > max || security.VerifyClosedReceipt(master, *receipt, installation, machine, epoch) != nil {
		return ErrTrafficReceipt
	}
	return nil
}

func heldContains(held []protocol.CloseRange, seq uint64) bool {
	for _, r := range held {
		if seq < r.First {
			return false
		}
		if seq <= r.Last {
			return true
		}
	}
	return false
}

func (s *Store) RecoverTraffic(ctx context.Context, machineID string, request protocol.TrafficRecoveryRequest, now time.Time) (protocol.TrafficRecoveryResponse, error) {
	response := protocol.TrafficRecoveryResponse{
		Version:        protocol.TrafficVersion,
		RequestID:      request.RequestID,
		AuthorityEpoch: request.AuthorityEpoch,
		MachineID:      machineID,
		LedgerEpoch:    request.LedgerEpoch,
		Resume:         false,
	}
	if request.ValidateEnvelope() != nil || request.MachineID != machineID {
		return response, ErrTrafficInvalid
	}
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return response, fmt.Errorf("begin traffic recovery: %w", err)
	}
	defer tx.Rollback()
	if err = validateTrafficAuthority(ctx, tx, machineID, request.AuthorityEpoch); err != nil {
		return response, err
	}
	installation, err := s.panelInstallationTx(ctx, tx)
	if err != nil {
		return response, err
	}
	ledger, err := readTrafficLedger(ctx, tx, machineID)
	if errors.Is(err, sql.ErrNoRows) {
		if request.Receipt != nil || request.InstallationID != "" && request.InstallationID != installation {
			return response, ErrTrafficEpoch
		}
		ledger = trafficLedger{MachineID: machineID, LedgerEpoch: request.LedgerEpoch, InstallationID: installation, Allocated: request.Allocated}
		if _, err = tx.ExecContext(ctx, `INSERT INTO traffic_ledgers
			(machine_id,ledger_epoch,installation_id,allocated_close_seq,closed_through,updated_at)
			VALUES(?,?,?,?,?,?)`, machineID, request.LedgerEpoch, installation, int64(request.Allocated), 0, trafficTextTime(now)); err != nil {
			return response, fmt.Errorf("create traffic ledger: %w", err)
		}
	} else if err != nil {
		return response, fmt.Errorf("read traffic ledger: %w", err)
	} else {
		if ledger.LedgerEpoch != request.LedgerEpoch {
			return response, ErrTrafficEpoch
		}
		if ledger.InstallationID != installation {
			return response, ErrTrafficEpoch
		}
		if request.InstallationID != "" && request.InstallationID != ledger.InstallationID {
			return response, ErrTrafficEpoch
		}
		if request.Allocated < ledger.Allocated {
			return response, ErrTrafficRecoveryGap
		}
		if request.Allocated > ledger.Allocated {
			ledger.Allocated = request.Allocated
			if err = writeTrafficLedger(ctx, tx, ledger, now); err != nil {
				return response, fmt.Errorf("record traffic allocation high-water: %w", err)
			}
		}
	}
	if err = validateTrafficReceipt(s.masterKey, request.Receipt, ledger.InstallationID, machineID, ledger.LedgerEpoch, ledger.ClosedThrough); err != nil {
		return response, err
	}
	if request.Allocated < ledger.Allocated {
		return response, ErrTrafficRecoveryGap
	}
	rows, err := tx.QueryContext(ctx, `SELECT close_seq FROM traffic_segments
		WHERE machine_id=? AND ledger_epoch=? AND finalized=1 AND close_seq IS NOT NULL`, machineID, ledger.LedgerEpoch)
	if err != nil {
		return response, fmt.Errorf("read traffic recovery finals: %w", err)
	}
	for rows.Next() {
		var seq int64
		if err = rows.Scan(&seq); err != nil {
			rows.Close()
			return response, err
		}
		if seq <= 0 || !heldContains(request.Held, uint64(seq)) {
			rows.Close()
			return response, ErrTrafficRecoveryGap
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return response, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return response, fmt.Errorf("commit traffic recovery: %w", err)
	}
	response.InstallationID = ledger.InstallationID
	response.LedgerEpoch = ledger.LedgerEpoch
	response.Resume = true
	if ledger.ClosedThrough > 0 {
		receipt, signErr := security.SignClosedReceipt(s.masterKey, security.ClosedClaims{
			Version: 1, InstallationID: ledger.InstallationID, MachineID: machineID,
			LedgerEpoch: ledger.LedgerEpoch, ClosedThrough: ledger.ClosedThrough,
		})
		if signErr != nil {
			return protocol.TrafficRecoveryResponse{}, signErr
		}
		response.Receipt = &receipt
	}
	return response, nil
}

func validateTrafficPolicies(ctx context.Context, tx *sql.Tx, segment protocol.TrafficSegment) error {
	for _, p := range segment.Periods {
		var encoded string
		if err := tx.QueryRowContext(ctx, "SELECT policy_json FROM period_policies WHERE policy_id=?", p.PolicyID).Scan(&encoded); errors.Is(err, sql.ErrNoRows) {
			return ErrTrafficPolicy
		} else if err != nil {
			return err
		} else {
			var policy period.Policy
			if err := json.Unmarshal([]byte(encoded), &policy); err != nil || policy.Validate() != nil ||
				policy.ID != p.PolicyID || policy.Scope != p.Scope || policy.Revision != p.PolicyRevision {
				return ErrTrafficPolicy
			}
			expected, err := period.At(policy, p.Start)
			if err != nil || expected.ID != p.ID || expected.Scope != p.Scope || expected.PolicyID != p.PolicyID ||
				expected.PolicyRevision != p.PolicyRevision || !expected.Start.Equal(p.Start) || !expected.End.Equal(p.End) {
				return ErrTrafficPolicy
			}
		}
	}
	return nil
}

func trafficSegmentJSON(segment protocol.TrafficSegment) (string, error) {
	encoded, err := json.Marshal(segment)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func loadTrafficSegment(ctx context.Context, tx *sql.Tx, query string, args ...any) (protocol.TrafficSegment, error) {
	var encoded string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&encoded); err != nil {
		return protocol.TrafficSegment{}, err
	}
	var segment protocol.TrafficSegment
	if err := json.Unmarshal([]byte(encoded), &segment); err != nil {
		return protocol.TrafficSegment{}, ErrTrafficInvalid
	}
	if err := segment.Validate(); err != nil {
		return protocol.TrafficSegment{}, ErrTrafficInvalid
	}
	return segment, nil
}

func closeSeqOwner(ctx context.Context, tx *sql.Tx, machine, epoch string, seq uint64) (protocol.SegmentKey, error) {
	var stream, segment string
	err := tx.QueryRowContext(ctx, `SELECT stream_id,segment_id FROM traffic_segments
		WHERE machine_id=? AND ledger_epoch=? AND close_seq=?
		UNION ALL SELECT stream_id,segment_id FROM traffic_closed_segments
		WHERE machine_id=? AND ledger_epoch=? AND close_seq=? LIMIT 1`, machine, epoch, int64(seq), machine, epoch, int64(seq)).Scan(&stream, &segment)
	if err != nil {
		return protocol.SegmentKey{}, err
	}
	return protocol.SegmentKey{MachineID: machine, LedgerEpoch: epoch, StreamID: stream, SegmentID: segment}, nil
}

func segmentIDOwner(ctx context.Context, tx *sql.Tx, machine, epoch, segmentID string) (protocol.SegmentKey, error) {
	var stream string
	err := tx.QueryRowContext(ctx, `SELECT stream_id FROM traffic_segments
		WHERE machine_id=? AND ledger_epoch=? AND segment_id=?
		UNION ALL SELECT stream_id FROM traffic_closed_segments
		WHERE machine_id=? AND ledger_epoch=? AND segment_id=? LIMIT 1`, machine, epoch, segmentID, machine, epoch, segmentID).Scan(&stream)
	if err != nil {
		return protocol.SegmentKey{}, err
	}
	return protocol.SegmentKey{MachineID: machine, LedgerEpoch: epoch, StreamID: stream, SegmentID: segmentID}, nil
}

func applyTrafficUsage(ctx context.Context, tx *sql.Tx, segment protocol.TrafficSegment, delta protocol.Totals, now time.Time) error {
	if delta.Uplink == 0 && delta.Downlink == 0 {
		return nil
	}
	for _, p := range segment.Periods {
		var oldUp, oldDown int64
		err := tx.QueryRowContext(ctx, `SELECT uplink,downlink FROM traffic_usage
			WHERE machine_id=? AND ledger_epoch=? AND scope_key=? AND period_id=?`,
			segment.Key.MachineID, segment.Key.LedgerEpoch, p.Scope.Key(), p.ID).Scan(&oldUp, &oldDown)
		if errors.Is(err, sql.ErrNoRows) {
			oldUp, oldDown = 0, 0
		} else if err != nil {
			return err
		}
		up, err := quantity.Add(quantity.Bytes(oldUp), delta.Uplink)
		if err != nil {
			return err
		}
		down, err := quantity.Add(quantity.Bytes(oldDown), delta.Downlink)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO traffic_usage
			(machine_id,ledger_epoch,scope_key,period_id,uplink,downlink,updated_at)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(machine_id,ledger_epoch,scope_key,period_id)
			DO UPDATE SET uplink=excluded.uplink,downlink=excluded.downlink,updated_at=excluded.updated_at`,
			segment.Key.MachineID, segment.Key.LedgerEpoch, p.Scope.Key(), p.ID, int64(up), int64(down), trafficTextTime(now)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ingestTrafficRow(ctx context.Context, tx *sql.Tx, ledger *trafficLedger, segment protocol.TrafficSegment, now time.Time) (protocol.TrafficOutcome, error) {
	outcome := protocol.TrafficOutcome{Key: segment.Key}
	if err := segment.Validate(); err != nil {
		return outcome, &trafficRowReject{code: trafficErrorInvalid}
	}
	closed, err := loadTrafficSegment(ctx, tx, `SELECT body FROM traffic_closed_segments
		WHERE machine_id=? AND ledger_epoch=? AND stream_id=? AND segment_id=?`,
		segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID)
	if err == nil {
		if err = protocol.ValidateCumulativeUpdate(closed, segment); err != nil {
			return outcome, &trafficRowReject{code: trafficErrorClosedConflict}
		}
		outcome.Status, outcome.CloseSeq = "retired", closed.CloseSeq
		return outcome, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return outcome, err
	}
	if owner, ownerErr := segmentIDOwner(ctx, tx, segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.SegmentID); ownerErr == nil && owner != segment.Key {
		return outcome, &trafficRowReject{code: trafficErrorSegmentConflict}
	} else if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
		return outcome, ownerErr
	}

	previous, err := loadTrafficSegment(ctx, tx, `SELECT body FROM traffic_segments
		WHERE machine_id=? AND ledger_epoch=? AND stream_id=? AND segment_id=?`,
		segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID)
	hasPrevious := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return outcome, err
	}
	if hasPrevious {
		if err = protocol.ValidateCumulativeUpdate(previous, segment); err != nil {
			return outcome, &trafficRowReject{code: trafficErrorSegmentConflict}
		}
	} else if err = validateTrafficPolicies(ctx, tx, segment); err != nil {
		if errors.Is(err, ErrTrafficPolicy) {
			return outcome, &trafficRowReject{code: trafficErrorUnknownPolicy}
		}
		return outcome, err
	}
	if segment.Finalized {
		owner, ownerErr := closeSeqOwner(ctx, tx, segment.Key.MachineID, segment.Key.LedgerEpoch, segment.CloseSeq)
		if ownerErr == nil && owner != segment.Key {
			return outcome, &trafficRowReject{code: trafficErrorCloseSeqConflict}
		}
		if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
			return outcome, ownerErr
		}
	}

	previousTotals := protocol.Totals{}
	if hasPrevious {
		previousTotals = previous.Totals
	}
	delta := protocol.Totals{Uplink: segment.Totals.Uplink - previousTotals.Uplink, Downlink: segment.Totals.Downlink - previousTotals.Downlink}
	if err = applyTrafficUsage(ctx, tx, segment, delta, now); err != nil {
		return outcome, err
	}
	encoded, err := trafficSegmentJSON(segment)
	if err != nil {
		return outcome, err
	}
	var closeSeq any
	if segment.Finalized {
		closeSeq = int64(segment.CloseSeq)
	}
	if hasPrevious {
		_, err = tx.ExecContext(ctx, `UPDATE traffic_segments SET body=?,accepted_up=?,accepted_down=?,finalized=?,close_seq=?,updated_at=?
			WHERE machine_id=? AND ledger_epoch=? AND stream_id=? AND segment_id=?`, encoded, int64(segment.Totals.Uplink), int64(segment.Totals.Downlink), boolInt(segment.Finalized), closeSeq, trafficTextTime(now), segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO traffic_segments
			(machine_id,ledger_epoch,stream_id,segment_id,body,accepted_up,accepted_down,finalized,close_seq,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID, encoded, int64(segment.Totals.Uplink), int64(segment.Totals.Downlink), boolInt(segment.Finalized), closeSeq, trafficTextTime(now))
	}
	if err != nil {
		return outcome, err
	}
	if segment.Finalized && segment.CloseSeq > ledger.Allocated {
		ledger.Allocated = segment.CloseSeq
	}
	outcome.Status = "cumulative"
	outcome.Committed = &protocol.Totals{Uplink: segment.Totals.Uplink, Downlink: segment.Totals.Downlink}
	if segment.Finalized {
		outcome.Status, outcome.CloseSeq = "final", segment.CloseSeq
	}
	return outcome, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func recordTrafficReport(ctx context.Context, tx *sql.Tx, request protocol.TrafficRequest, row int, segment protocol.TrafficSegment, outcome protocol.TrafficOutcome, now time.Time) error {
	encoded, err := json.Marshal(segment)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_reports
		(machine_id,ledger_epoch,request_id,row_index,segment_id,body,status,error_code,received_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, request.MachineID, request.LedgerEpoch, request.RequestID, row, segment.Key.SegmentID,
		string(encoded), outcome.Status, outcome.ErrorCode, trafficTextTime(now))
	return err
}

func (s *Store) advanceTrafficPrefix(ctx context.Context, tx *sql.Tx, ledger *trafficLedger, now time.Time) error {
	for ledger.ClosedThrough < ledger.Allocated {
		next := ledger.ClosedThrough + 1
		segment, err := loadTrafficSegment(ctx, tx, `SELECT body FROM traffic_segments
			WHERE machine_id=? AND ledger_epoch=? AND finalized=1 AND close_seq=?`, ledger.MachineID, ledger.LedgerEpoch, int64(next))
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		encoded, err := trafficSegmentJSON(segment)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO traffic_closed_segments
			(machine_id,ledger_epoch,stream_id,segment_id,close_seq,body,closed_at)
			VALUES(?,?,?,?,?,?,?)`, segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID, int64(segment.CloseSeq), encoded, trafficTextTime(now)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_segments
			WHERE machine_id=? AND ledger_epoch=? AND stream_id=? AND segment_id=?`, segment.Key.MachineID, segment.Key.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID); err != nil {
			return err
		}
		ledger.ClosedThrough = next
	}
	return nil
}

func (s *Store) IngestTraffic(ctx context.Context, machineID string, request protocol.TrafficRequest, rows []protocol.TrafficRow, now time.Time) (protocol.TrafficACK, error) {
	ack := protocol.TrafficACK{Version: protocol.TrafficVersion, RequestID: request.RequestID, AuthorityEpoch: request.AuthorityEpoch, MachineID: machineID, LedgerEpoch: request.LedgerEpoch, Outcomes: []protocol.TrafficOutcome{}}
	if request.ValidateEnvelope() != nil || request.MachineID != machineID || len(rows) != len(request.Segments) {
		return ack, ErrTrafficInvalid
	}
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ack, fmt.Errorf("begin traffic ingest: %w", err)
	}
	defer tx.Rollback()
	if err = validateTrafficAuthority(ctx, tx, machineID, request.AuthorityEpoch); err != nil {
		return ack, err
	}
	installation, err := s.panelInstallationTx(ctx, tx)
	if err != nil {
		return ack, err
	}
	ledger, err := readTrafficLedger(ctx, tx, machineID)
	if errors.Is(err, sql.ErrNoRows) {
		return ack, ErrTrafficEpoch
	}
	if err != nil {
		return ack, err
	}
	if ledger.LedgerEpoch != request.LedgerEpoch {
		return ack, ErrTrafficEpoch
	}
	if err = validateTrafficReceipt(s.masterKey, request.PreviousReceipt, installation, machineID, ledger.LedgerEpoch, ledger.ClosedThrough); err != nil {
		return ack, err
	}
	for i, row := range rows {
		outcome := protocol.TrafficOutcome{Row: i, Key: row.Segment.Key}
		if row.Err != nil {
			outcome.Status, outcome.ErrorCode = "rejected", trafficErrorInvalid
			if err = recordTrafficReport(ctx, tx, request, i, row.Segment, outcome, now); err != nil {
				return ack, err
			}
			ack.Outcomes = append(ack.Outcomes, outcome)
			continue
		}
		if row.Segment.Key.MachineID != machineID || row.Segment.Key.LedgerEpoch != ledger.LedgerEpoch {
			outcome.Status, outcome.ErrorCode = "rejected", trafficErrorInvalid
			if err = recordTrafficReport(ctx, tx, request, i, row.Segment, outcome, now); err != nil {
				return ack, err
			}
			ack.Outcomes = append(ack.Outcomes, outcome)
			continue
		}
		result, rowErr := s.ingestTrafficRow(ctx, tx, &ledger, row.Segment, now)
		if rowErr != nil {
			var rejected *trafficRowReject
			if errors.As(rowErr, &rejected) {
				outcome.Status, outcome.ErrorCode = "rejected", rejected.code
				if err = recordTrafficReport(ctx, tx, request, i, row.Segment, outcome, now); err != nil {
					return ack, err
				}
				ack.Outcomes = append(ack.Outcomes, outcome)
				continue
			}
			return ack, rowErr
		}
		result.Row = i
		if err = recordTrafficReport(ctx, tx, request, i, row.Segment, result, now); err != nil {
			return ack, err
		}
		ack.Outcomes = append(ack.Outcomes, result)
	}
	if err = s.advanceTrafficPrefix(ctx, tx, &ledger, now); err != nil {
		return ack, err
	}
	if err = writeTrafficLedger(ctx, tx, ledger, now); err != nil {
		return ack, fmt.Errorf("persist traffic ledger: %w", err)
	}
	for i := range ack.Outcomes {
		if ack.Outcomes[i].Status == "final" && ack.Outcomes[i].CloseSeq <= ledger.ClosedThrough {
			ack.Outcomes[i].Status = "retired"
			ack.Outcomes[i].Committed = nil
		}
	}
	if err = tx.Commit(); err != nil {
		return protocol.TrafficACK{}, fmt.Errorf("commit traffic ingest: %w", err)
	}
	if ledger.ClosedThrough > 0 {
		receipt, signErr := security.SignClosedReceipt(s.masterKey, security.ClosedClaims{
			Version: 1, InstallationID: installation, MachineID: machineID,
			LedgerEpoch: ledger.LedgerEpoch, ClosedThrough: ledger.ClosedThrough,
		})
		if signErr != nil {
			return protocol.TrafficACK{}, signErr
		}
		ack.Receipt = &receipt
	}
	return ack, nil
}
