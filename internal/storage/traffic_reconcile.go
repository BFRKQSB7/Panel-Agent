package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrTrafficGapNotPending = errors.New("traffic reconciliation is not pending")
	ErrTrafficGapEvidence   = errors.New("traffic recovery gap evidence is invalid")
	ErrTrafficGapRange      = errors.New("traffic recovery gap range is invalid")
	ErrTrafficGapHasDetails = errors.New("traffic recovery gap still has Panel details")
	ErrTrafficGapHeld       = errors.New("traffic recovery gap is still held by Agent")
	ErrTrafficGapOverlap    = errors.New("traffic recovery gap overlaps a recorded gap")
)

type TrafficRecoveryGapInput struct {
	MachineID      string
	LedgerEpoch    string
	AgentAllocated uint64
	FirstSeq       uint64
	LastSeq        uint64
	Held           []protocol.CloseRange
	Receipt        security.ClosedReceipt
}

type TrafficRecoveryGap struct {
	MachineID             string
	LedgerEpoch           string
	FirstSeq              uint64
	LastSeq               uint64
	EvidenceClosedThrough uint64
	EvidenceDigest        string
	RecordedAt            string
}

// RecordTrafficRecoveryGap advances only a consecutive missing close prefix.
// It requires a master-key-verifiable receipt from the Agent and rejects any
// range that is still present in Panel or explicitly held by the Agent. No
// bytes are added to usage by this operation.
func (s *Store) RecordTrafficRecoveryGap(ctx context.Context, input TrafficRecoveryGapInput, now time.Time) error {
	if input.Receipt.Version == 2 && input.LastSeq != input.Receipt.ClosedThrough {
		return ErrTrafficGapRange
	}
	if !security.IsUUIDv4(input.MachineID) || !security.IsUUIDv4(input.LedgerEpoch) || input.FirstSeq == 0 || input.LastSeq < input.FirstSeq || input.LastSeq > 1<<63-1 || input.AgentAllocated < input.LastSeq || input.Receipt.ClosedThrough < input.LastSeq || input.Receipt.ClosedThrough > input.AgentAllocated {
		return ErrTrafficGapRange
	}
	if input.Receipt.MachineID != input.MachineID || input.Receipt.LedgerEpoch != input.LedgerEpoch || input.Receipt.ClosedClaims.Validate() != nil {
		return ErrTrafficGapEvidence
	}
	for _, held := range input.Held {
		if held.First == 0 || held.Last < held.First || held.Last > input.AgentAllocated {
			return ErrTrafficGapEvidence
		}
		if held.First <= input.LastSeq && held.Last >= input.FirstSeq {
			return ErrTrafficGapHeld
		}
	}
	evidenceBytes, err := json.Marshal(input.Receipt)
	if err != nil {
		return err
	}
	evidenceHash := sha256.Sum256(evidenceBytes)
	evidenceDigest := hex.EncodeToString(evidenceHash[:])
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ledger, err := readTrafficLedger(ctx, tx, input.MachineID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTrafficEpoch
	}
	if err != nil {
		return err
	}
	if ledger.LedgerEpoch != input.LedgerEpoch {
		return ErrTrafficEpoch
	}
	installation, err := s.panelInstallationTx(ctx, tx)
	if err != nil {
		return err
	}
	if security.VerifyClosedReceipt(s.masterKey, input.Receipt, installation, input.MachineID, input.LedgerEpoch) != nil {
		return ErrTrafficGapEvidence
	}
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT evidence_digest FROM traffic_recovery_gaps
		WHERE machine_id=? AND ledger_epoch=? AND first_seq=? AND last_seq=?`, input.MachineID, input.LedgerEpoch, int64(input.FirstSeq), int64(input.LastSeq)).Scan(&existingDigest)
	if err == nil {
		if existingDigest != evidenceDigest {
			return ErrTrafficGapEvidence
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !ledger.ReconciliationPending {
		return ErrTrafficGapNotPending
	}
	if input.FirstSeq != ledger.ClosedThrough+1 {
		return ErrTrafficGapRange
	}
	var overlap int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_recovery_gaps
		WHERE machine_id=? AND ledger_epoch=? AND last_seq>=? AND first_seq<=?`, input.MachineID, input.LedgerEpoch, int64(input.FirstSeq), int64(input.LastSeq)).Scan(&overlap); err != nil {
		return err
	}
	if overlap != 0 {
		return ErrTrafficGapOverlap
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM traffic_segments WHERE machine_id=? AND ledger_epoch=? AND close_seq>=? AND close_seq<=?`,
		`SELECT COUNT(*) FROM traffic_closed_segments WHERE machine_id=? AND ledger_epoch=? AND close_seq>=? AND close_seq<=?`,
	} {
		var count int
		if err := tx.QueryRowContext(ctx, query, input.MachineID, input.LedgerEpoch, int64(input.FirstSeq), int64(input.LastSeq)).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrTrafficGapHasDetails
		}
	}
	allocated := ledger.Allocated
	if input.AgentAllocated > allocated {
		allocated = input.AgentAllocated
	}
	if allocated > 1<<63-1 {
		return ErrTrafficGapRange
	}
	pending := 0
	if input.LastSeq < allocated {
		pending = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_recovery_gaps(machine_id,ledger_epoch,first_seq,last_seq,evidence_closed_through,evidence_digest,recorded_at) VALUES(?,?,?,?,?,?,?)`, input.MachineID, input.LedgerEpoch, int64(input.FirstSeq), int64(input.LastSeq), int64(input.Receipt.ClosedThrough), evidenceDigest, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE traffic_ledgers SET allocated_close_seq=?,closed_through=?,reconciliation_pending=?,updated_at=? WHERE machine_id=?`, int64(allocated), int64(input.LastSeq), pending, now.Format(time.RFC3339Nano), input.MachineID); err != nil {
		return err
	}
	if input.Receipt.Version == 2 {
		ranges, err := security.ParseClosedOrdinals(input.Receipt.ClosedOrdinals)
		if err != nil {
			return ErrTrafficGapEvidence
		}
		for _, r := range ranges {
			if err = retireOrdinalRangeTx(ctx, tx, input.MachineID, input.LedgerEpoch, r.First, r.Last); err != nil {
				return err
			}
		}
		// Proof may cover a cumulative K present in the old backup whose final
		// was acknowledged later. It is closed evidence, never a new charge.
		rows, err := tx.QueryContext(ctx, `SELECT segment_id FROM traffic_segments WHERE machine_id=? AND ledger_epoch=?`, input.MachineID, input.LedgerEpoch)
		if err != nil {
			return err
		}
		var retired []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ordinal := protocol.TrafficOrdinal(id)
			for _, r := range ranges {
				if ordinal >= r.First && ordinal <= r.Last {
					retired = append(retired, id)
					break
				}
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range retired {
			if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_segments WHERE machine_id=? AND ledger_epoch=? AND segment_id=?`, input.MachineID, input.LedgerEpoch, id); err != nil {
				return err
			}
		}
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListTrafficRecoveryGaps(ctx context.Context, machineID string) ([]TrafficRecoveryGap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id,ledger_epoch,first_seq,last_seq,evidence_closed_through,evidence_digest,recorded_at
		FROM traffic_recovery_gaps WHERE machine_id=? ORDER BY ledger_epoch,first_seq`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TrafficRecoveryGap{}
	for rows.Next() {
		var item TrafficRecoveryGap
		var first, last, through int64
		if err := rows.Scan(&item.MachineID, &item.LedgerEpoch, &first, &last, &through, &item.EvidenceDigest, &item.RecordedAt); err != nil {
			return nil, err
		}
		if first <= 0 || last < first || through <= 0 {
			return nil, fmt.Errorf("invalid persisted traffic recovery gap")
		}
		item.FirstSeq, item.LastSeq, item.EvidenceClosedThrough = uint64(first), uint64(last), uint64(through)
		result = append(result, item)
	}
	return result, rows.Err()
}
