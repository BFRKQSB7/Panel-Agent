package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

func (m *CoreManager) proveSampleProcess(record *coreConfigRecord) error {
	if m.state.Process == nil || m.state.Process.ConfigPath != m.configPath(record) {
		return errors.New("Core sampling identity is not established")
	}
	actual, err := inspectCoreProcess(m.state.Process.PID, m.binary)
	if err != nil || actual != *m.state.Process {
		return errors.New("Core identity changed during sampling")
	}
	return nil
}

func ledgerReadings(counters []boundaryCounter) []ledger.Reading {
	readings := []ledger.Reading{}
	for _, c := range counters {
		// Explicit development credentials are never attributed to a formal user.
		if c.UserID == "development" {
			continue
		}
		readings = append(readings, ledger.Reading{Label: c.Email, Stream: protocol.TrafficStream{Kind: "user_entry", UserID: c.UserID, EntryNodeID: c.NodeID}, Totals: protocol.Totals{Uplink: quantity.Bytes(c.Uplink), Downlink: quantity.Bytes(c.Downlink)}})
	}
	return readings
}

func (m *CoreManager) consumeBoundaryFile(ctx context.Context, name string, cleanup bool) error {
	if m.ledger == nil {
		return nil
	}
	if !validBoundaryName(name) {
		return errors.New("invalid boundary name")
	}
	path := filepath.Join(m.dir, "boundaries", name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return errors.New("invalid boundary file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var b coreBoundary
	if err = json.Unmarshal(contents, &b); err != nil {
		return err
	}
	if b.Config == nil {
		return errors.New("boundary mapping missing")
	}
	developmentOnly := len(b.Config.Mappings) > 0
	for _, mapping := range b.Config.Mappings {
		developmentOnly = developmentOnly && mapping.UserID == "development"
	}
	if developmentOnly {
		return nil
	} // Keep bounded, non-billable D04 test evidence.
	if len(b.Config.PeriodPolicies) == 0 {
		// Pre-D08 observations have no published period identity. Retain the raw
		// file for reconciliation; never invent a historical policy to bill it.
		return nil
	}
	sample := ledger.Sample{ImportID: name, SourceID: b.InstanceID, Generation: b.Generation, Started: b.SampleStartedAt, Finished: b.SampleFinishedAt, PolicyAppliedAt: b.Config.PolicyAppliedAt, Policies: b.Config.PeriodPolicies, Readings: ledgerReadings(b.Counters), Finalize: true, Gap: b.ObservationGap, HistoricalBoundary: true}
	if err = m.ledger.Ingest(ctx, sample); err != nil {
		return err
	}
	if !cleanup || b.StoppedAt.IsZero() && !b.ApplyAbortedRunning {
		return nil
	}
	if !b.StoppedAt.IsZero() {
		if err = m.ledger.RecordGap(ctx, "final_sample_to_stop_unobserved", b.SampleStartedAt, b.StoppedAt); err != nil {
			return err
		}
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	return m.ledger.ForgetImport(ctx, name, b.InstanceID, !b.StoppedAt.IsZero())
}

func (m *CoreManager) consumeCompletedBoundaries(ctx context.Context) error {
	if m.ledger == nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(m.dir, "boundaries"))
	if err != nil {
		return err
	}
	type pending struct {
		name string
		at   time.Time
	}
	queue := []pending{}
	for _, entry := range entries {
		if !validBoundaryName(entry.Name()) || entry.Name() == m.state.Boundary {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, "boundaries", entry.Name()))
		if err != nil {
			return err
		}
		var b coreBoundary
		if len(data) > 4<<20 || json.Unmarshal(data, &b) != nil {
			return errors.New("invalid retained boundary")
		}
		if b.StoppedAt.IsZero() && !b.ApplyAbortedRunning && b.InstanceID == m.state.InstanceID && b.Generation == m.state.Generation && b.Config != nil && len(b.Config.PeriodPolicies) > 0 {
			// The observation can reach disk before its journal reference does.
			// Recover that owned snapshot too; its ledger import remains idempotent.
			if m.proveSampleProcess(b.Config) == nil {
				b.ApplyAbortedRunning = true
			} else {
				b.StoppedAt = time.Now().UTC()
				b.UnobservedTail = true
				b.ObservationGap = "orphan_boundary_core_continuity_unproven"
			}
			if err = m.writeBoundary(entry.Name(), b); err != nil {
				return err
			}
		}
		if !b.StoppedAt.IsZero() || b.ApplyAbortedRunning {
			queue = append(queue, pending{entry.Name(), b.SampleFinishedAt})
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].at.Before(queue[j].at) })
	for _, p := range queue {
		if err = m.consumeBoundaryFile(ctx, p.name, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *CoreManager) Sample(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ledger == nil || m.state.Current == nil {
		return nil
	}
	record := m.state.Current
	if len(record.PeriodPolicies) == 0 {
		return m.ledger.RecordGap(ctx, "legacy_config_without_period_policy", time.Now().UTC(), time.Now().UTC())
	}
	started := time.Now().UTC()
	if err := m.proveSampleProcess(record); err != nil {
		_ = m.ledger.RecordGap(ctx, "core_sampling_identity_unproven", started, time.Now().UTC())
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	stats, err := xray.DialStats(probe, "127.0.0.1:10085")
	if err != nil {
		return err
	}
	defer stats.Close()
	counters := []boundaryCounter{}
	for _, mapping := range record.Mappings {
		if mapping.UserID == "development" {
			continue
		}
		if !security.IsUUIDv4(mapping.UserID) {
			return errors.New("unknown formal usage mapping")
		}
		counter := boundaryCounter{StatMapping: mapping}
		for _, d := range []string{"uplink", "downlink"} {
			v, e := stats.QueryCounter(probe, "user>>>"+mapping.Email+">>>traffic>>>"+d)
			if errors.Is(e, xray.ErrStatNotFound) {
				v, e = 0, nil
			}
			if e != nil || v < 0 {
				return errors.New("invalid cumulative Core counter")
			}
			if d == "uplink" {
				counter.Uplink = v
			} else {
				counter.Downlink = v
			}
		}
		counters = append(counters, counter)
	}
	if err = m.proveSampleProcess(record); err != nil {
		return err
	}
	err = m.ledger.Ingest(ctx, ledger.Sample{SourceID: m.state.InstanceID, Generation: m.state.Generation, Started: started, Finished: time.Now().UTC(), PolicyAppliedAt: record.PolicyAppliedAt, Policies: record.PeriodPolicies, Readings: ledgerReadings(counters)})
	if err != nil {
		_ = m.ledger.RecordGap(ctx, "sample_not_committed", started, time.Now().UTC())
	}
	return err
}

func (m *CoreManager) nextSample() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	wait := ledger.SampleInterval
	if m.ledger == nil {
		return wait
	}
	now := time.Now().UTC()
	next := m.ledger.NextBoundary(context.Background())
	if m.state.Current != nil {
		for _, p := range m.state.Current.PeriodPolicies {
			if p.EffectiveAt.After(now) && (next.IsZero() || p.EffectiveAt.Before(next)) {
				next = p.EffectiveAt
			}
		}
	}
	if !next.IsZero() {
		wait = min(wait, max(time.Until(next), 100*time.Millisecond))
	}
	return wait
}
