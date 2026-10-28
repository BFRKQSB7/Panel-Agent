package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

// hysteriaSession serializes one live instance with its durable counter source.
// It is owned by the Agent coordinator, which also holds the machine data lock.
// The lifecycle context must outlive individual Panel requests.
type hysteriaSession struct {
	mu       sync.Mutex
	instance *hysteria.Instance
	source   *hysteriaSource
}

// recoverHysteriaSessions runs once before any new auth listener or instance is
// started. A new Agent never adopts a former process's unproven counter source.
func recoverHysteriaSessions(ctx context.Context, dir string, book *ledger.Store, binary string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "hysteria-source-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "hysteria-source-"), ".json")
		if !security.IsUUIDv4(id) {
			return errors.New("hysteria: unexpected source journal name")
		}
		if err := hysteria.StopOrphans(ctx, binary, filepath.Join(dir, "hysteria", id)); err != nil {
			return err
		}
		source, err := loadHysteriaSource(ctx, filepath.Join(dir, entry.Name()), book)
		if err != nil {
			return err
		}
		at := time.Now().UTC()
		if at.Before(source.state.LastAt) {
			at = source.state.LastAt
		}
		if err := source.recoverStopped(ctx, at); err != nil {
			return err
		}
		if err := source.retire(ctx); err != nil {
			return err
		}
	}
	return nil
}

func startHysteriaSession(ctx context.Context, dir string, book *ledger.Store, binary string, node protocol.DesiredNode, policies []period.Policy, restrictions protocol.QuotaRestrictions, options hysteria.ServerOptions, generation uint64) (*hysteriaSession, error) {
	at := time.Now().UTC()
	accounts, err := hysteriaAccounts(node, restrictions, at)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("hysteria: no authorized accounts to serve")
	}
	root, err := storage.PrepareDataDir(dir)
	if err != nil {
		return nil, err
	}
	options.InstanceDir = filepath.Join(root, "hysteria", node.ID)
	if _, err := storage.PrepareDataDir(options.InstanceDir); err != nil {
		return nil, err
	}
	options.ListenHost, options.ListenPort = node.ListenHost, node.ListenPort
	// Metadata must reach disk and the common ledger before a byte is proxied.
	source, err := newHysteriaSource(ctx, root, book, node, policies, generation, at)
	if err != nil {
		return nil, err
	}
	instance, err := hysteria.StartInstance(ctx, binary, options, accounts)
	if err != nil {
		// Retain the prepared source. A failed startup is not proof that every
		// possible child has exited; recovery must establish that separately.
		return nil, err
	}
	return &hysteriaSession{instance: instance, source: source}, nil
}

func (s *hysteriaSession) sample(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	readings, err := s.instance.Snapshot(ctx)
	if err == nil {
		err = s.source.record(ctx, readings, time.Now().UTC(), false, "")
	}
	if err == nil {
		return nil
	}
	// Never keep serving indefinitely when usage cannot be attributed or stored.
	return errors.Join(err, s.stopLocked())
}

func (s *hysteriaSession) stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.source.state.Closed {
		return nil
	}
	readings, err := s.instance.Snapshot(ctx)
	if err == nil {
		err = s.source.record(ctx, readings, time.Now().UTC(), false, "")
	}
	return errors.Join(err, s.stopLocked())
}

func (s *hysteriaSession) stopLocked() error {
	// Safety cleanup has its own bound even if a Panel request was cancelled.
	cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := s.instance.Stop(cleanup); err != nil {
		return err
	}
	// Snapshot-before-stop cannot observe the exact final tail of a killed Core.
	// Retain measured bytes, explicitly mark the uncertainty, and only then GC
	// the obsolete source watermarks. Unacknowledged segments are never removed.
	return s.source.recoverStopped(cleanup, time.Now().UTC())
}
