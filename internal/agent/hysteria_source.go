package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type hysteriaObservation struct {
	ID       string
	At       time.Time
	Counters map[string]hysteria.Counter
	Final    bool
	Gap      string
}

type hysteriaSourceState struct {
	Version         int
	NodeID          string
	SourceID        string
	Generation      uint64
	Started         time.Time
	PolicyAppliedAt time.Time
	Policies        []period.Policy
	// Only attribution fields are persisted here; no proxy credentials.
	Mappings      []protocol.ProxyCredential
	LastAt        time.Time
	Last          map[string]hysteria.Counter
	Pending       *hysteriaObservation
	CleanupImport string
	Closed        bool
}

// hysteriaSource is a bounded write-ahead journal for one owned Core instance.
// The Agent's data lock must be held by its owner. Source creation precedes
// process launch; closed journals may only be replaced after confirmed exit.
type hysteriaSource struct {
	mu    sync.Mutex
	path  string
	book  *ledger.Store
	state hysteriaSourceState
}

func newHysteriaSource(ctx context.Context, dir string, book *ledger.Store, node protocol.DesiredNode, policies []period.Policy, generation uint64, at time.Time) (*hysteriaSource, error) {
	if book == nil || !security.IsUUIDv4(node.ID) || generation == 0 || generation > 1<<63-1 || !period.ValidUTC(at) || len(policies) == 0 || len(policies) > 4096 {
		return nil, errors.New("hysteria: invalid source metadata")
	}
	if _, err := hysteriaReadings(node.ID, node.Credentials, nil); err != nil {
		return nil, err
	}
	for i, p := range policies {
		if err := period.CheckPublication(policies[:i], p); err != nil {
			return nil, err
		}
	}
	root, err := storage.PrepareDataDir(dir)
	if err != nil {
		return nil, err
	}
	if err := storage.RequireOwner(root); err != nil {
		return nil, err
	}
	p := filepath.Join(root, "hysteria-source-"+node.ID+".json")
	if _, err := os.Lstat(p); err == nil {
		return nil, errors.New("hysteria: previous source must be recovered and retired first")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	id, err := security.NewUUID()
	if err != nil {
		return nil, err
	}
	s := &hysteriaSource{path: p, book: book, state: hysteriaSourceState{Version: 1, NodeID: node.ID, SourceID: id, Generation: generation, Started: at, LastAt: at, PolicyAppliedAt: at, Policies: policies, Last: map[string]hysteria.Counter{}}}
	for _, c := range node.Credentials {
		s.state.Mappings = append(s.state.Mappings, protocol.ProxyCredential{UserID: c.UserID, Email: c.Email})
	}
	// JSON round-trip makes all slices independent of the caller's snapshot.
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &s.state); err != nil {
		return nil, err
	}
	if err := s.save(s.state); err != nil {
		return nil, err
	}
	if err := book.RegisterSource(ctx, id, generation, at, true); err != nil {
		return nil, err
	}
	return s, nil
}

func loadHysteriaSource(ctx context.Context, p string, book *ledger.Store) (*hysteriaSource, error) {
	if book == nil {
		return nil, errors.New("hysteria: missing durable ledger")
	}
	if err := storage.RequireOwner(p); err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("hysteria: unsafe source journal")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	s := &hysteriaSource{path: p, book: book}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, errors.New("hysteria: invalid source journal")
	}
	v := s.state
	if v.Version != 1 || !security.IsUUIDv4(v.NodeID) || !security.IsUUIDv4(v.SourceID) || v.Generation == 0 || v.Generation > 1<<63-1 || !period.ValidUTC(v.Started) || !period.ValidUTC(v.LastAt) || v.LastAt.Before(v.Started) || len(v.Policies) == 0 || len(v.Policies) > 4096 || len(v.Mappings) > 4096 || len(v.Last) > 4096 || v.Closed && v.Pending != nil {
		return nil, errors.New("hysteria: inconsistent source journal")
	}
	if !period.ValidUTC(v.PolicyAppliedAt) || v.CleanupImport != "" && !security.IsUUIDv4(v.CleanupImport) || v.Pending != nil && v.CleanupImport != "" {
		return nil, errors.New("hysteria: invalid source cleanup metadata")
	}
	for i, p := range v.Policies {
		if err := period.CheckPublication(v.Policies[:i], p); err != nil {
			return nil, err
		}
	}
	if filepath.Base(p) != "hysteria-source-"+v.NodeID+".json" {
		return nil, errors.New("hysteria: source path identity mismatch")
	}
	for _, c := range v.Mappings {
		if c.UUID != "" || c.ExpiresAt != "" {
			return nil, errors.New("hysteria: secret found in source mapping")
		}
	}
	if _, err := hysteriaReadings(v.NodeID, v.Mappings, v.Last); err != nil {
		return nil, err
	}
	if !v.Closed {
		if err := book.RegisterSource(ctx, v.SourceID, v.Generation, v.Started, true); err != nil {
			return nil, err
		}
	}
	if err := s.replay(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *hysteriaSource) save(next hysteriaSourceState) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("hysteria: source journal too large")
	}
	if err := storage.WritePrivateAtomic(s.path, data); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *hysteriaSource) cleanup(ctx context.Context) error {
	if s.state.CleanupImport == "" {
		return nil
	}
	if err := s.book.ForgetImport(ctx, s.state.CleanupImport, s.state.SourceID, s.state.Closed); err != nil {
		return err
	}
	next := s.state
	next.CleanupImport = ""
	return s.save(next)
}

func (s *hysteriaSource) replay(ctx context.Context) error {
	if err := s.cleanup(ctx); err != nil {
		return err
	}
	o := s.state.Pending
	if o == nil {
		return nil
	}
	if !security.IsUUIDv4(o.ID) || !period.ValidUTC(o.At) || o.At.Before(s.state.LastAt) || len(o.Counters) > 4096 || len(o.Gap) > 128 {
		return errors.New("hysteria: invalid pending observation")
	}
	for id, previous := range s.state.Last {
		current, exists := o.Counters[id]
		if !exists || current.TX < previous.TX || current.RX < previous.RX {
			return errors.New("hysteria: counter continuity lost")
		}
	}
	readings, err := hysteriaReadings(s.state.NodeID, s.state.Mappings, o.Counters)
	if err != nil {
		return err
	}
	if err := s.book.Ingest(ctx, ledger.Sample{ImportID: o.ID, SourceID: s.state.SourceID, Generation: s.state.Generation, Started: s.state.Started, Finished: o.At, PolicyAppliedAt: s.state.PolicyAppliedAt, Policies: s.state.Policies, Readings: readings, Finalize: o.Final, Gap: o.Gap, HistoricalBoundary: true}); err != nil {
		return err
	}
	next := s.state
	next.Last = o.Counters
	next.LastAt = o.At
	next.Pending = nil
	next.Closed = o.Final
	next.CleanupImport = o.ID
	if err := s.save(next); err != nil {
		return err
	}
	return s.cleanup(ctx)
}

// retire removes only a closed source journal. Finalized unacknowledged usage
// stays in the common ledger, while a subsequent process receives a fresh UUID.
func (s *hysteriaSource) retire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replay(ctx); err != nil {
		return err
	}
	if !s.state.Closed {
		return errors.New("hysteria: refusing to retire an open source")
	}
	if err := os.Remove(s.path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(s.path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

// record journals the whole observation before changing ledger watermarks. A
// failed ingest is replayed before any newer observation can supersede it.
// final is only allowed after the supervisor has confirmed the process exited.
func (s *hysteriaSource) record(ctx context.Context, counters map[string]hysteria.Counter, at time.Time, final bool, gap string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(ctx, counters, at, final, gap)
}

func (s *hysteriaSource) recordLocked(ctx context.Context, counters map[string]hysteria.Counter, at time.Time, final bool, gap string) error {
	if err := s.replay(ctx); err != nil {
		return err
	}
	if s.state.Closed {
		return errors.New("hysteria: source already closed")
	}
	if !period.ValidUTC(at) || at.Before(s.state.LastAt) || len(counters) > 4096 || len(gap) > 128 {
		return errors.New("hysteria: invalid observation time")
	}
	if _, err := hysteriaReadings(s.state.NodeID, s.state.Mappings, counters); err != nil {
		return err
	}
	for id, previous := range s.state.Last {
		current, exists := counters[id]
		if !exists || current.TX < previous.TX || current.RX < previous.RX {
			return errors.New("hysteria: counter reset requires a new source")
		}
	}
	id, err := security.NewUUID()
	if err != nil {
		return err
	}
	copyCounters := make(map[string]hysteria.Counter, len(counters))
	for id, c := range counters {
		copyCounters[id] = c
	}
	next := s.state
	next.Pending = &hysteriaObservation{ID: id, At: at, Counters: copyCounters, Final: final, Gap: gap}
	if err := s.save(next); err != nil {
		return err
	}
	return s.replay(ctx)
}

// recoverStopped closes an orphaned source after the supervisor has established
// that its child is gone. No invented final counter is added for the unknown tail.
func (s *hysteriaSource) recoverStopped(ctx context.Context, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replay(ctx); err != nil {
		return err
	}
	if s.state.Closed {
		return nil
	}
	return s.recordLocked(ctx, s.state.Last, at, true, "hysteria_process_tail_unobserved")
}
