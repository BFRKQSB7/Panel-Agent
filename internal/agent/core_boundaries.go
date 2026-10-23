package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type boundaryCounter struct {
	xray.StatMapping
	Uplink   int64 `json:"uplink"`
	Downlink int64 `json:"downlink"`
}

// These are cumulative observations, not billable deltas. The later durable
// ledger must deduplicate by Core instance and retain explicit observation gaps.
type coreBoundary struct {
	Version             int               `json:"version"`
	Generation          uint64            `json:"core_generation"`
	InstanceID          string            `json:"core_instance_id"`
	Config              *coreConfigRecord `json:"config"`
	SampleStartedAt     time.Time         `json:"sample_started_at"`
	SampleFinishedAt    time.Time         `json:"sample_finished_at"`
	StoppedAt           time.Time         `json:"stopped_at,omitempty"`
	Counters            []boundaryCounter `json:"counters"`
	ObservationGap      string            `json:"observation_gap,omitempty"`
	UnobservedTail      bool              `json:"unobserved_tail"`
	ApplyAbortedRunning bool              `json:"apply_aborted_current_continues,omitempty"`
}

func (m *CoreManager) captureBoundary(ctx context.Context, disappeared bool) (string, error) {
	return m.captureRecordBoundary(ctx, m.state.Current, disappeared)
}

func (m *CoreManager) captureRecordBoundary(ctx context.Context, record *coreConfigRecord, disappeared bool) (string, error) {
	if record == nil {
		return "", nil
	}
	if m.state.Boundary != "" {
		return m.state.Boundary, nil
	}
	dir := filepath.Join(m.dir, "boundaries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	// Never discard unconsumed usage mappings to make space. Until D08 consumes
	// them, refuse a new restart once this bounded queue reaches its ceiling.
	count := 0
	for _, entry := range entries {
		if validBoundaryName(entry.Name()) {
			count++
		}
	}
	limit := 126
	if record == m.state.Candidate {
		limit = 128
	}
	if count >= limit {
		return "", errors.New("Core boundary queue is full; retained observations need ledger consumption")
	}
	boundary := coreBoundary{Version: 1, Generation: m.state.Generation, InstanceID: m.state.InstanceID,
		Config: record, SampleStartedAt: time.Now().UTC(), Counters: []boundaryCounter{}}
	if disappeared {
		boundary.ObservationGap, boundary.UnobservedTail = "core_disappeared_before_final_sample", true
	} else {
		probe, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		stats, err := xray.DialStats(probe, "127.0.0.1:10085")
		if err != nil {
			return "", err
		}
		defer stats.Close()
		for _, mapping := range record.Mappings {
			counter := boundaryCounter{StatMapping: mapping}
			for _, direction := range []string{"uplink", "downlink"} {
				value, err := stats.QueryCounter(probe, "user>>>"+mapping.Email+">>>traffic>>>"+direction)
				if errors.Is(err, xray.ErrStatNotFound) {
					value, err = 0, nil
				}
				if err != nil || value < 0 {
					return "", fmt.Errorf("cannot capture final Core counters for %s: %w", mapping.NodeID, err)
				}
				if direction == "uplink" {
					counter.Uplink = value
				} else {
					counter.Downlink = value
				}
			}
			boundary.Counters = append(boundary.Counters, counter)
		}
	}
	boundary.SampleFinishedAt = time.Now().UTC()
	id, err := security.NewUUID()
	if err != nil {
		return "", err
	}
	name := "boundary-" + id + ".json"
	if err := m.writeBoundary(name, boundary); err != nil {
		return "", err
	}
	next := m.state
	next.Boundary = name
	if err := m.save(next); err != nil {
		return "", err
	}
	return name, nil
}

func (m *CoreManager) captureCandidate(ctx context.Context, process coreProcess) error {
	if m.state.Candidate == nil || process.ConfigPath != m.configPath(m.state.Candidate) {
		return nil
	}
	// A candidate can accept traffic before health commits it. Retain its labels
	// too; an unavailable Stats API is an explicit gap rather than invented zeros.
	if _, err := m.captureRecordBoundary(ctx, m.state.Candidate, false); err != nil {
		if _, gapErr := m.captureRecordBoundary(ctx, m.state.Candidate, true); gapErr != nil {
			return gapErr
		}
	}
	return nil
}

func (m *CoreManager) captureLostCandidate(ctx context.Context, found bool) error {
	if found || m.state.Candidate == nil {
		return nil
	}
	switch m.state.Phase {
	case "starting", "checking", "restoring":
	default:
		return nil
	}
	name, err := m.captureRecordBoundary(ctx, m.state.Candidate, true)
	if err != nil {
		return err
	}
	return m.finishBoundary(name, false)
}

func validBoundaryName(name string) bool {
	return strings.HasPrefix(name, "boundary-") && strings.HasSuffix(name, ".json") &&
		security.IsUUIDv4(strings.TrimSuffix(strings.TrimPrefix(name, "boundary-"), ".json"))
}

func (m *CoreManager) writeBoundary(name string, boundary coreBoundary) error {
	if !validBoundaryName(name) {
		return errors.New("invalid Core boundary filename")
	}
	contents, err := json.MarshalIndent(boundary, "", "  ")
	if err != nil {
		return err
	}
	if len(contents) > 4<<20 {
		return errors.New("Core boundary exceeds size limit")
	}
	return storage.WritePrivateAtomic(filepath.Join(m.dir, "boundaries", name), contents)
}

func (m *CoreManager) finishBoundary(name string, stillRunning bool) error {
	if name == "" {
		return nil
	}
	if !validBoundaryName(name) {
		return errors.New("invalid pending Core boundary")
	}
	contents, err := os.ReadFile(filepath.Join(m.dir, "boundaries", name))
	if err != nil {
		return err
	}
	var boundary coreBoundary
	if len(contents) > 4<<20 || json.Unmarshal(contents, &boundary) != nil {
		return errors.New("invalid persisted Core boundary")
	}
	if boundary.StoppedAt.IsZero() && !boundary.ApplyAbortedRunning {
		if stillRunning {
			boundary.ApplyAbortedRunning = true
		} else {
			boundary.StoppedAt, boundary.UnobservedTail = time.Now().UTC(), true
			// Sampling and shutdown are not atomic in the pinned Stats API.
			// The interval starting at SampleStartedAt is explicitly uncertain.
			if boundary.ObservationGap == "" {
				boundary.ObservationGap = "final_sample_to_process_stop_not_atomic"
			}
		}
		if err := m.writeBoundary(name, boundary); err != nil {
			return err
		}
	}
	next := m.state
	next.Boundary = ""
	return m.save(next)
}
