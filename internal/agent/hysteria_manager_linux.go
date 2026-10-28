package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type hysteriaRecord struct {
	Version    int
	Authority  string
	Revision   uint64
	Digest     string
	Desired    protocol.DesiredState
	Generation uint64
	Pending    bool
	AppliedAt  time.Time
}

type hysteriaManager struct {
	mu          sync.Mutex
	dir, binary string
	book        *ledger.Store
	lifecycle   context.Context
	cancel      context.CancelFunc
	record      hysteriaRecord
	sessions    map[string]*hysteriaSession
	hashes      map[string]string
	lastError   error
	instanceID  string
}

func newHysteriaManager(ctx context.Context, dir, binary string, book *ledger.Store) (*hysteriaManager, error) {
	if ctx == nil || book == nil || !filepath.IsAbs(binary) {
		return nil, errors.New("hysteria: invalid manager configuration")
	}
	// Pin the actual official artifact, not a version string supplied by an
	// arbitrary executable. Upgrade support must explicitly extend this allowlist.
	if err := hysteria.VerifyOfficialBinary(binary, runtime.GOARCH); err != nil {
		return nil, err
	}
	root, err := storage.PrepareDataDir(dir)
	if err != nil {
		return nil, err
	}
	if err := storage.RequireOwner(root); err != nil {
		return nil, err
	}
	if err := recoverHysteriaSessions(ctx, root, book, binary); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	instanceID, err := security.NewUUID()
	if err != nil {
		cancel()
		return nil, err
	}
	m := &hysteriaManager{dir: root, binary: binary, book: book, lifecycle: life, cancel: cancel, sessions: map[string]*hysteriaSession{}, hashes: map[string]string{}, record: hysteriaRecord{Version: 1}}
	m.instanceID = instanceID
	m.record.Generation = 1
	p := filepath.Join(root, "hysteria-state.json")
	if info, err := os.Lstat(p); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 2<<20 {
			cancel()
			return nil, errors.New("hysteria: unsafe manager journal")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			cancel()
			return nil, err
		}
		if err := json.Unmarshal(data, &m.record); err != nil {
			cancel()
			return nil, errors.New("hysteria: invalid manager journal")
		}
		digest, err := protocol.DigestDesiredState(m.record.Desired)
		if m.record.Version != 1 || err != nil || digest != m.record.Digest || !security.IsUUIDv4(m.record.Authority) || m.record.Revision == 0 {
			cancel()
			return nil, errors.New("hysteria: manager snapshot identity mismatch")
		}
		if m.record.Generation >= 1<<63-1 {
			cancel()
			return nil, errors.New("hysteria: generation exhausted")
		}
		next := m.record
		next.Generation++
		if err := m.save(next); err != nil {
			cancel()
			return nil, err
		}
		// Interrupted changes stay stopped until a fresh authenticated full sync;
		// recovery never rolls an old permission snapshot back into service.
		if !m.record.Pending {
			if err := m.applyLocked(ctx, m.record.Authority, m.record.Revision, m.record.Digest, m.record.Desired); err != nil {
				m.lastError = err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cancel()
		return nil, err
	}
	return m, nil
}

func (m *hysteriaManager) save(next hysteriaRecord) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return errors.New("hysteria: manager snapshot too large")
	}
	if err := storage.WritePrivateAtomic(filepath.Join(m.dir, "hysteria-state.json"), data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &m.record); err != nil {
		return err
	}
	return nil
}

func localAPIPort() (string, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := l.Addr().String()
	return address, l.Close()
}

func (m *hysteriaManager) Apply(ctx context.Context, authority string, revision uint64, digest string, desired protocol.DesiredState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.applyLocked(ctx, authority, revision, digest, desired)
	m.lastError = err
	return err
}

func (m *hysteriaManager) applyLocked(ctx context.Context, authority string, revision uint64, digest string, desired protocol.DesiredState) error {
	actual, err := protocol.DigestDesiredState(desired)
	if err != nil || actual != digest || revision == 0 || !security.IsUUIDv4(authority) {
		return errors.New("hysteria: desired identity invalid")
	}
	if err := protocol.ValidateDesiredState(desired); err != nil {
		return err
	}
	if m.record.Authority == authority && (revision < m.record.Revision || revision == m.record.Revision && digest != m.record.Digest) {
		return errors.New("hysteria: stale desired revision")
	}
	nodes := map[string]protocol.DesiredNode{}
	options := map[string]hysteria.ServerOptions{}
	hashes := map[string]string{}
	for _, node := range desired.Nodes {
		if node.Extension == nil || node.Extension.Kind != "hysteria2_tls" {
			continue
		}
		accounts, err := hysteriaAccounts(node, desired.QuotaRestrictions, time.Now().UTC())
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			continue
		}
		encoded, _ := json.Marshal(struct {
			Node         protocol.DesiredNode
			Accounts     []hysteria.Account
			Policies     any
			Restrictions protocol.QuotaRestrictions
		}{node, accounts, desired.PeriodPolicies, desired.QuotaRestrictions})
		h := sha256.Sum256(encoded)
		hashes[node.ID] = hex.EncodeToString(h[:])
		nodes[node.ID] = node
		auth, err := localAPIPort()
		if err != nil {
			return err
		}
		stats, err := localAPIPort()
		if err != nil {
			return err
		}
		for stats == auth {
			stats, err = localAPIPort()
			if err != nil {
				return err
			}
		}
		route, err := security.NewToken(32)
		if err != nil {
			return err
		}
		secret, err := security.NewToken(32)
		if err != nil {
			return err
		}
		o := hysteria.ServerOptions{ListenHost: node.ListenHost, ListenPort: node.ListenPort, ServerName: node.Extension.SNI, CertificatePEM: node.Extension.CertificatePEM, PrivateKeyPEM: node.Extension.PrivateKeyPEM, InstanceDir: filepath.Join(m.dir, "hysteria", node.ID), AuthAddress: auth, AuthRouteToken: route, StatsAddress: stats, StatsSecret: secret}
		if _, err := hysteria.RenderServer(o); err != nil {
			return err
		}
		options[node.ID] = o
	}
	changed := m.record.Pending || m.record.Digest != digest || m.record.Authority != authority || m.record.Revision != revision
	for id, s := range m.sessions {
		if hashes[id] != m.hashes[id] {
			changed = true
			continue
		}
		if _, err := s.instance.Snapshot(ctx); err != nil {
			changed = true
		}
	}
	for id := range nodes {
		if m.sessions[id] == nil {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	next := hysteriaRecord{Version: 1, Authority: authority, Revision: revision, Digest: digest, Desired: desired, Generation: m.record.Generation, Pending: true}
	if err := m.save(next); err != nil {
		return err
	}
	for id, s := range m.sessions {
		keep := hashes[id] == m.hashes[id]
		if keep {
			_, err := s.instance.Snapshot(ctx)
			keep = err == nil
		}
		if keep {
			continue
		}
		err := s.stop(ctx)
		if !s.source.state.Closed {
			return err
		}
		if err := s.source.retire(ctx); err != nil {
			return err
		}
		delete(m.sessions, id)
		delete(m.hashes, id)
	}
	for id, node := range nodes {
		if m.sessions[id] != nil {
			continue
		}
		p := filepath.Join(m.dir, "hysteria-source-"+id+".json")
		if _, err := os.Lstat(p); err == nil {
			if err := hysteria.StopOrphans(ctx, m.binary, options[id].InstanceDir); err != nil {
				return err
			}
			s, err := loadHysteriaSource(ctx, p, m.book)
			if err != nil {
				return err
			}
			at := time.Now().UTC()
			if at.Before(s.state.LastAt) {
				at = s.state.LastAt
			}
			if err := s.recoverStopped(ctx, at); err != nil {
				return err
			}
			if err := s.retire(ctx); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next = m.record
		if next.Generation >= 1<<63-1 {
			return errors.New("hysteria: generation exhausted")
		}
		next.Generation++
		if err := m.save(next); err != nil {
			return err
		}
		s, err := startHysteriaSession(m.lifecycle, m.dir, m.book, m.binary, node, desired.PeriodPolicies, desired.QuotaRestrictions, options[id], next.Generation)
		if err != nil {
			return err
		}
		m.sessions[id], m.hashes[id] = s, hashes[id]
	}
	next = m.record
	next.Pending = false
	next.AppliedAt = time.Now().UTC()
	return m.save(next)
}

func (m *hysteriaManager) Sample(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for _, s := range m.sessions {
		result = errors.Join(result, s.sample(ctx))
	}
	if result != nil {
		m.lastError = result
	}
	return result
}

func (m *hysteriaManager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record.Digest == "" || m.record.Pending {
		return nil
	}
	err := m.applyLocked(ctx, m.record.Authority, m.record.Revision, m.record.Digest, m.record.Desired)
	m.lastError = err
	return err
}

func (m *hysteriaManager) Matches(authority string, revision uint64, digest string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastError != nil || m.record.Pending || m.record.Authority != authority || m.record.Revision != revision || m.record.Digest != digest {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, s := range m.sessions {
		if _, err := s.instance.Snapshot(ctx); err != nil {
			return false
		}
	}
	return true
}

func (m *hysteriaManager) Status() CoreApplyStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record
	restriction := ""
	if !r.AppliedAt.IsZero() {
		restriction, _ = r.Desired.QuotaRestrictions.Digest()
	}
	status := CoreApplyStatus{Generation: max(1, r.Generation), InstanceID: m.instanceID, AuthorityEpoch: r.Authority, Revision: r.Revision, Digest: r.Digest, RestrictionDigest: restriction, Running: !r.Pending && m.lastError == nil}
	status.RestrictionAppliedAt = r.AppliedAt
	if r.Pending {
		status.AuthorityEpoch = ""
		status.Revision = 0
		status.Digest = ""
		status.RestrictionDigest = ""
		status.RestrictionAppliedAt = time.Time{}
	}
	if m.lastError != nil {
		status.Error = protocol.ErrorCoreStartFailed
	}
	return status
}

func (m *hysteriaManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range m.sessions {
		_ = s.stop(ctx)
	}
	m.cancel()
}
