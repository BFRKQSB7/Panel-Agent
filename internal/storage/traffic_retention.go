package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

// Limits apply to acknowledged history; pending accounting is never evicted.
type TrafficRetention struct {
	RawDays     int `json:"raw_days"`
	RawRows     int `json:"raw_rows_per_machine"`
	ClosedRows  int `json:"closed_rows_per_machine"`
	HourDays    int `json:"hour_days"`
	DayDays     int `json:"day_days"`
	MonthMonths int `json:"month_months"`
}

func retireOrdinalRangeTx(ctx context.Context, tx *sql.Tx, machine, epoch string, first, last uint64) error {
	if err := tx.QueryRowContext(ctx, `SELECT MIN(first),MAX(last) FROM (SELECT first,last FROM traffic_retired_ordinals WHERE machine_id=? AND ledger_epoch=? AND first<=? AND last>=? UNION ALL SELECT ?,?)`, machine, epoch, last+1, first-1, first, last).Scan(&first, &last); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_retired_ordinals WHERE machine_id=? AND ledger_epoch=? AND first<=? AND last>=?`, machine, epoch, last, first); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO traffic_retired_ordinals VALUES(?,?,?,?)`, machine, epoch, first, last)
	return err
}

func trafficReceiptClaims(ctx context.Context, tx *sql.Tx, ledger trafficLedger) (security.ClosedClaims, error) {
	claims := security.ClosedClaims{Version: 1, InstallationID: ledger.InstallationID, MachineID: ledger.MachineID, LedgerEpoch: ledger.LedgerEpoch, ClosedThrough: ledger.ClosedThrough}
	if ledger.ClosedThrough == 0 {
		return claims, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT first,last,'' FROM traffic_retired_ordinals WHERE machine_id=? AND ledger_epoch=? UNION ALL SELECT 0,0,segment_id FROM traffic_closed_segments WHERE machine_id=? AND ledger_epoch=?`, ledger.MachineID, ledger.LedgerEpoch, ledger.MachineID, ledger.LedgerEpoch)
	if err != nil {
		return claims, err
	}
	var ranges []security.ClosedOrdinalRange
	for rows.Next() {
		var r security.ClosedOrdinalRange
		var id string
		if err = rows.Scan(&r.First, &r.Last, &id); err != nil {
			break
		}
		if id != "" {
			r.First = protocol.TrafficOrdinal(id)
			r.Last = r.First
		}
		if r.First > 0 {
			ranges = append(ranges, r)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return claims, err
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].First < ranges[j].First })
	var merged []security.ClosedOrdinalRange
	for _, r := range ranges {
		if len(merged) > 0 && r.First <= merged[len(merged)-1].Last+1 {
			if r.Last > merged[len(merged)-1].Last {
				merged[len(merged)-1].Last = r.Last
			}
		} else {
			merged = append(merged, r)
		}
	}
	if len(merged) > 0 {
		var parts []string
		for _, r := range merged {
			parts = append(parts, fmt.Sprintf("%d-%d", r.First, r.Last))
		}
		claims.Version = 2
		claims.ClosedOrdinals = strings.Join(parts, ",")
	}
	return claims, claims.Validate()
}

func (r TrafficRetention) Validate() error {
	if r.RawDays < 1 || r.RawDays > 30 || r.RawRows < 1 || r.RawRows > 100000 || r.ClosedRows < 1 || r.ClosedRows > 10000 || r.HourDays < 1 || r.HourDays > 31 || r.DayDays < 1 || r.DayDays > 366 || r.MonthMonths < 1 || r.MonthMonths > 120 {
		return ErrTrafficInvalid
	}
	return nil
}

type trafficQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readTrafficRetention(ctx context.Context, q trafficQuerier) (TrafficRetention, error) {
	var r TrafficRetention
	err := q.QueryRowContext(ctx, `SELECT raw_days,raw_rows,closed_rows,hour_days,day_days,month_months FROM traffic_retention WHERE id=1`).Scan(&r.RawDays, &r.RawRows, &r.ClosedRows, &r.HourDays, &r.DayDays, &r.MonthMonths)
	if err != nil {
		return r, err
	}
	return r, r.Validate()
}

func (s *Store) GetTrafficRetention(ctx context.Context) (TrafficRetention, error) {
	return readTrafficRetention(ctx, s.db)
}
func (s *Store) SaveTrafficRetention(ctx context.Context, r TrafficRetention) error {
	if err := r.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE traffic_retention SET raw_days=?,raw_rows=?,closed_rows=?,hour_days=?,day_days=?,month_months=? WHERE id=1`, r.RawDays, r.RawRows, r.ClosedRows, r.HourDays, r.DayDays, r.MonthMonths)
	return err
}

func trafficBuckets(at time.Time) map[string]time.Time {
	at = at.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	return map[string]time.Time{"hour": at.Truncate(time.Hour), "day": day, "month": time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)}
}

func recordTrafficRollups(ctx context.Context, tx *sql.Tx, segment protocol.TrafficSegment, delta protocol.Totals, now time.Time) error {
	// These are observation buckets, not reconstructed per-hour traffic curves.
	for _, p := range segment.Periods {
		for resolution, bucket := range trafficBuckets(now) {
			var up, down int64
			err := tx.QueryRowContext(ctx, `SELECT uplink,downlink FROM traffic_rollups WHERE machine_id=? AND ledger_epoch=? AND scope_key=? AND resolution=? AND bucket=?`, segment.Key.MachineID, segment.Key.LedgerEpoch, p.Scope.Key(), resolution, trafficTextTime(bucket)).Scan(&up, &down)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			u, err := quantity.Add(quantity.Bytes(up), delta.Uplink)
			if err != nil {
				return err
			}
			d, err := quantity.Add(quantity.Bytes(down), delta.Downlink)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO traffic_rollups(machine_id,ledger_epoch,scope_key,resolution,bucket,uplink,downlink,backfilled,estimated) VALUES(?,?,?,?,?,?,?,?,1)
		 ON CONFLICT(machine_id,ledger_epoch,scope_key,resolution,bucket) DO UPDATE SET uplink=excluded.uplink,downlink=excluded.downlink,backfilled=MAX(backfilled,excluded.backfilled)`, segment.Key.MachineID, segment.Key.LedgerEpoch, p.Scope.Key(), resolution, trafficTextTime(bucket), int64(u), int64(d), boolInt(now.Sub(segment.Through) > time.Hour))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Legacy UUIDv4 details remain exact migration tombstones. New UUIDv8 creation
// ordinals collapse into ranges independently of finalization order and time.
func compactTrafficTx(ctx context.Context, tx *sql.Tx, ledger trafficLedger, now time.Time) error {
	r, err := readTrafficRetention(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT stream_id,segment_id,body FROM traffic_closed_segments WHERE machine_id=? AND ledger_epoch=? AND substr(segment_id,1,24)='00000000-0000-8000-8000-' AND
	 (closed_at<? OR close_seq<=COALESCE((SELECT close_seq FROM traffic_closed_segments WHERE machine_id=? AND ledger_epoch=? ORDER BY close_seq DESC LIMIT 1 OFFSET ?),0)) ORDER BY close_seq LIMIT 256`, ledger.MachineID, ledger.LedgerEpoch, trafficTextTime(now.AddDate(0, 0, -r.RawDays)), ledger.MachineID, ledger.LedgerEpoch, r.ClosedRows)
	if err != nil {
		return err
	}
	var segments []protocol.TrafficSegment
	for rows.Next() {
		var stream, id, body string
		if err = rows.Scan(&stream, &id, &body); err != nil {
			rows.Close()
			return err
		}
		var segment protocol.TrafficSegment
		e := json.Unmarshal([]byte(body), &segment)
		if e != nil || segment.Validate() != nil {
			rows.Close()
			return ErrTrafficInvalid
		}
		segments = append(segments, segment)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if segment.CloseSeq > ledger.ClosedThrough {
			return ErrTrafficInvalid
		}
		ordinal := protocol.TrafficOrdinal(segment.Key.SegmentID)
		if ordinal == 0 {
			continue
		}
		if err = retireOrdinalRangeTx(ctx, tx, ledger.MachineID, ledger.LedgerEpoch, ordinal, ordinal); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_closed_segments WHERE machine_id=? AND ledger_epoch=? AND stream_id=? AND segment_id=?`, ledger.MachineID, ledger.LedgerEpoch, segment.Key.StreamID, segment.Key.SegmentID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_reports WHERE report_id IN (SELECT report_id FROM traffic_reports WHERE machine_id=? AND
	 (received_at<? OR report_id<=COALESCE((SELECT report_id FROM traffic_reports WHERE machine_id=? ORDER BY report_id DESC LIMIT 1 OFFSET ?),0)) ORDER BY report_id LIMIT 1000)`, ledger.MachineID, trafficTextTime(now.AddDate(0, 0, -r.RawDays)), ledger.MachineID, r.RawRows); err != nil {
		return err
	}
	cutoffs := map[string]time.Time{"hour": now.AddDate(0, 0, -r.HourDays), "day": now.AddDate(0, 0, -r.DayDays), "month": trafficBuckets(now)["month"].AddDate(0, -r.MonthMonths, 0)}
	for resolution, cutoff := range cutoffs {
		if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_rollups WHERE resolution=? AND bucket<?`, resolution, trafficTextTime(trafficBuckets(cutoff)[resolution])); err != nil {
			return err
		}
	}
	// Unknown legacy period metadata remains quarantined, never inferred from
	// today's policy. Active/unacknowledged rows protect their period totals.
	_, err = tx.ExecContext(ctx, `DELETE FROM traffic_usage WHERE period_end<>'' AND period_end<=? AND NOT EXISTS
	 (SELECT 1 FROM traffic_segments AS s,json_each(s.body,'$.periods') AS p WHERE s.machine_id=traffic_usage.machine_id AND s.ledger_epoch=traffic_usage.ledger_epoch AND json_extract(p.value,'$.period_id')=traffic_usage.period_id)`, trafficTextTime(cutoffs["month"]))
	return err
}
