package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type coreConfigRecord struct {
	AuthorityEpoch       string             `json:"authority_epoch"`
	Revision             uint64             `json:"revision"`
	Digest               string             `json:"digest"`
	ConfigHash           string             `json:"config_hash"`
	Mappings             []xray.StatMapping `json:"mappings"`
	Listeners            []string           `json:"listeners"`
	HasSource            bool               `json:"has_source,omitempty"`
	PeriodPolicies       []period.Policy    `json:"period_policies,omitempty"`
	PolicyAppliedAt      time.Time          `json:"policy_applied_at,omitempty"`
	RestrictionDigest    string             `json:"restriction_digest,omitempty"`
	RestrictionAppliedAt time.Time          `json:"restriction_applied_at,omitempty"`
}

type coreJournal struct {
	Version             int               `json:"version"`
	Binary              string            `json:"binary"`
	Phase               string            `json:"phase"`
	Generation          uint64            `json:"core_generation"`
	InstanceID          string            `json:"core_instance_id"`
	Current             *coreConfigRecord `json:"current"`
	LKG                 *coreConfigRecord `json:"lkg"`
	Candidate           *coreConfigRecord `json:"candidate"`
	Process             *coreProcess      `json:"process"`
	LastError           string            `json:"last_error,omitempty"`
	Boundary            string            `json:"pending_boundary,omitempty"`
	DevelopmentLoopback bool              `json:"development_loopback_test"`
	PermissionDigest    string            `json:"permission_digest,omitempty"`
	BinaryVersion       string            `json:"binary_version,omitempty"`
}

type CoreApplyStatus struct {
	Generation           uint64    `json:"core_generation"`
	InstanceID           string    `json:"core_instance_id"`
	AuthorityEpoch       string    `json:"applied_authority_epoch"`
	Revision             uint64    `json:"applied_revision"`
	Digest               string    `json:"applied_digest"`
	RestrictionDigest    string    `json:"applied_restriction_digest"`
	RestrictionAppliedAt time.Time `json:"restriction_applied_at,omitempty"`
	Running              bool      `json:"running"`
	Error                string    `json:"apply_error,omitempty"`
}

// CoreManager is the single serialized boundary for apply, recovery and later
// restart/upgrade operations. Agent's data lock provides cross-process exclusion.
type CoreManager struct {
	mu                  sync.Mutex
	dir                 string
	binary              string
	logPath             string
	binaryVersion       string
	coreOwned           bool
	state               coreJournal
	developmentLoopback bool
	ledger              *ledger.Store
	// Used only by subprocess tests to interrupt durable upgrade phases.
	upgradeCheckpoint func(string)
}

func NewCoreManager(dataDir, binaryPath string, developmentLoopback bool) (*CoreManager, error) {
	if !managedCoreSupported {
		return nil, errors.New("受管 Core 需要 Linux；Windows 可运行 Panel 与控制通道测试")
	}
	absolute, err := storage.PrepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	if err := storage.RequireOwner(absolute); err != nil {
		return nil, err
	}
	binary, err := filepath.Abs(binaryPath)
	if err != nil {
		return nil, err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, err
	}
	coreOwned, err := readCoreOwnership(absolute, binary)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(absolute, "core")
	for _, path := range []string{dir, filepath.Join(dir, "boundaries")} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("Core 数据目录必须为权限 0700 的真实目录")
		}
	}
	if err := migrateCoreBinaryReference(absolute, dir, binary, developmentLoopback); err != nil {
		return nil, err
	}
	if err := recoverPersistedUpgrade(dir, binary); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, err := readCoreVersion(ctx, binary)
	if err != nil {
		return nil, errors.New("Core 适配器不接受该 Xray 版本")
	}
	m := &CoreManager{dir: dir, binary: binary, binaryVersion: version, coreOwned: coreOwned, logPath: filepath.Join(dir, "core.log"), developmentLoopback: developmentLoopback}
	if err := m.load(); err != nil {
		return nil, err
	}
	if m.state.BinaryVersion != version {
		next := m.state
		next.BinaryVersion = version
		if err := m.save(next); err != nil {
			return nil, err
		}
	}
	return m, nil
}

type limitedCoreOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedCoreOutput) Write(value []byte) (int, error) {
	n := len(value)
	if remaining := b.limit - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(value[:min(remaining, len(value))])
	}
	return n, nil
}

func (m *CoreManager) configPath(record *coreConfigRecord) string {
	return filepath.Join(m.dir, "config-"+record.ConfigHash+".json")
}

func (m *CoreManager) load() error {
	path := filepath.Join(m.dir, "state.json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("Core journal must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m.save(coreJournal{Version: 1, Binary: m.binary, Phase: "stable", DevelopmentLoopback: m.developmentLoopback})
	}
	if err != nil {
		return err
	}
	var state coreJournal
	if len(contents) > 4<<20 {
		return errors.New("Core journal exceeds its size limit")
	}
	if err := json.Unmarshal(contents, &state); err != nil {
		return err
	}
	if state.Version != 1 || state.Binary != m.binary || state.Generation > 1<<63-1 || state.DevelopmentLoopback != m.developmentLoopback {
		return errors.New("Core journal identity or binary does not match")
	}
	if state.BinaryVersion != "" && !coreVersionSupported(state.BinaryVersion) {
		return errors.New("Core journal version is not supported")
	}
	if state.Generation > 0 && !security.IsUUIDv4(state.InstanceID) {
		return errors.New("Core journal has no valid generation identity")
	}
	if state.PermissionDigest != "" && !security.IsValidDigest(state.PermissionDigest) {
		return errors.New("invalid confirmed permission source")
	}
	switch state.Phase {
	case "stable", "validating", "prepared", "stopping", "starting", "checking", "restoring":
	default:
		return errors.New("unknown persisted Core phase")
	}
	for _, record := range []*coreConfigRecord{state.Current, state.LKG, state.Candidate} {
		if record != nil && (!security.IsUUIDv4(record.AuthorityEpoch) || record.Revision == 0 || !security.IsValidDigest(record.Digest) || !security.IsValidDigest(record.ConfigHash) || record.RestrictionDigest != "" && !security.IsValidDigest(record.RestrictionDigest)) {
			return errors.New("invalid persisted Core config reference")
		}
	}
	if state.Boundary != "" && !validBoundaryName(state.Boundary) {
		return errors.New("invalid persisted Core boundary reference")
	}
	m.state = state
	return nil
}

func (m *CoreManager) save(state coreJournal) error {
	if state.Current != nil {
		copy := *state.Current
		if m.state.Current == nil {
			copy.PolicyAppliedAt = time.Now().UTC()
		} else {
			before, _ := json.Marshal(m.state.Current.PeriodPolicies)
			after, _ := json.Marshal(copy.PeriodPolicies)
			if !bytes.Equal(before, after) {
				copy.PolicyAppliedAt = time.Now().UTC()
			} else if copy.PolicyAppliedAt.IsZero() {
				copy.PolicyAppliedAt = m.state.Current.PolicyAppliedAt
			}
		}
		if m.state.Current == nil {
			if copy.RestrictionAppliedAt.IsZero() {
				copy.RestrictionAppliedAt = time.Now().UTC()
			}
		} else if copy.RestrictionDigest != m.state.Current.RestrictionDigest {
			copy.RestrictionAppliedAt = time.Now().UTC()
		} else if copy.RestrictionAppliedAt.IsZero() {
			copy.RestrictionAppliedAt = m.state.Current.RestrictionAppliedAt
		}
		state.Current = &copy
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := storage.WritePrivateAtomic(filepath.Join(m.dir, "state.json"), contents); err != nil {
		return err
	}
	m.state = state
	return nil
}

func (m *CoreManager) verifyConfig(record *coreConfigRecord) error {
	info, err := os.Lstat(m.configPath(record))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return errors.New("Core config must be a private, bounded regular file")
	}
	contents, err := os.ReadFile(m.configPath(record))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(contents)
	if hex.EncodeToString(digest[:]) != record.ConfigHash {
		return errors.New("persisted Core config hash mismatch")
	}
	return nil
}

func (m *CoreManager) ownedProcesses() ([]coreProcess, error) {
	paths := map[string]bool{}
	for _, record := range []*coreConfigRecord{m.state.Current, m.state.LKG, m.state.Candidate} {
		if record != nil {
			paths[m.configPath(record)] = true
		}
	}
	return findCoreProcesses(m.binary, paths)
}

func (m *CoreManager) health(ctx context.Context, process coreProcess, record *coreConfigRecord, stable bool) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var lastErr error
	for {
		actual, err := inspectCoreProcess(process.PID, m.binary)
		if err != nil || actual != process || process.ConfigPath != m.configPath(record) {
			return errors.New("Core process exited or changed identity")
		}
		lastErr = nil
		for _, address := range record.Listeners {
			connection, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", address)
			if err != nil {
				lastErr = err
				break
			}
			_ = connection.Close()
		}
		if lastErr == nil {
			probeContext, probeCancel := context.WithTimeout(ctx, 400*time.Millisecond)
			stats, err := xray.DialStats(probeContext, "127.0.0.1:10085")
			if err == nil {
				uptime, pingErr := stats.Uptime(probeContext)
				_ = stats.Close()
				if pingErr == nil && (!stable || uptime >= 1) {
					probeCancel()
					return nil
				}
				lastErr = pingErr
			} else {
				lastErr = err
			}
			probeCancel()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("Core health check: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (m *CoreManager) start(ctx context.Context, record *coreConfigRecord, phase string) (coreProcess, error) {
	if m.state.Generation >= 1<<63-1 {
		return coreProcess{}, errors.New("Core generation exhausted")
	}
	if err := m.verifyConfig(record); err != nil {
		return coreProcess{}, err
	}
	id, err := security.NewUUID()
	if err != nil {
		return coreProcess{}, err
	}
	next := m.state
	next.Generation++
	next.InstanceID, next.Process, next.Phase = id, nil, phase
	if m.ledger != nil {
		if err := m.ledger.RegisterSource(ctx, id, next.Generation, time.Now().UTC(), true); err != nil {
			return coreProcess{}, err
		}
	}
	if err := m.save(next); err != nil {
		return coreProcess{}, err
	}
	process, err := startCoreCommand(m.binary, m.configPath(record), m.logPath)
	if err != nil {
		return coreProcess{}, err
	}
	next = m.state
	next.Process = &process
	if phase == "starting" {
		next.Phase = "checking"
	}
	if err := m.save(next); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		_ = stopCoreProcess(cleanup, process, m.binary)
		return coreProcess{}, err
	}
	return process, nil
}

func (m *CoreManager) restore(ctx context.Context) error {
	var restored *coreConfigRecord
	for index, original := range []*coreConfigRecord{m.state.Current, m.state.LKG} {
		if original == nil || restored != nil && restored.ConfigHash == original.ConfigHash {
			continue
		}
		restored = original
		isLKG := index == 1
		record, err := m.recordAtPresent(original)
		if err != nil {
			continue
		}
		next := m.state
		next.Candidate = record
		next.Phase = "validating"
		if err := m.save(next); err != nil {
			return err
		}
		if err := m.testConfig(ctx, record); err != nil {
			continue
		}
		process, err := m.start(ctx, record, "restoring")
		if err == nil {
			err = m.health(ctx, process, record, true)
		}
		if err != nil {
			if process.PID != 0 {
				if err := m.captureCandidate(ctx, process); err != nil {
					return err
				}
				if stopErr := stopCoreProcess(ctx, process, m.binary); stopErr != nil {
					return stopErr
				}
				if err := m.finishBoundary(m.state.Boundary, false); err != nil {
					return err
				}
			}
			continue
		}
		next = m.state
		next.Current, next.Candidate, next.Phase = record, nil, "stable"
		if isLKG {
			next.LKG = record
			next.LastError = protocol.ErrorCoreRecoveryFailed
		}
		return m.save(next)
	}
	return errors.New("no previous Core config could be restored")
}

func (m *CoreManager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	return m.recoverLocked(ctx)
}

func (m *CoreManager) recoverLocked(ctx context.Context) error {
	if err := m.consumeCompletedBoundaries(ctx); err != nil {
		return err
	}
	processes, err := m.ownedProcesses()
	if err != nil {
		return err
	}
	var current *coreProcess
	candidateFound := false
	for _, process := range processes {
		if m.state.Candidate != nil && process.ConfigPath == m.configPath(m.state.Candidate) {
			candidateFound = true
		}
		if m.state.Current != nil && process.ConfigPath == m.configPath(m.state.Current) && current == nil {
			copy := process
			current = &copy
			continue
		}
		if err := m.captureCandidate(ctx, process); err != nil {
			return err
		}
		if err := stopCoreProcess(ctx, process, m.binary); err != nil {
			return err
		}
		if err := m.finishBoundary(m.state.Boundary, false); err != nil {
			return err
		}
	}
	if err := m.captureLostCandidate(ctx, candidateFound); err != nil {
		return err
	}
	if m.state.Current == nil {
		next := m.state
		next.Candidate, next.Process, next.Phase = nil, nil, "stable"
		if err := m.save(next); err != nil {
			return err
		}
		return m.prune()
	}
	if current != nil && m.health(ctx, *current, m.state.Current, false) == nil {
		if m.state.Process == nil || *current != *m.state.Process {
			// Matching config path is not evidence that this is the same process.
			if m.ledger != nil {
				if err := m.ledger.RecordGap(ctx, "core_identity_continuity_unproven", time.Now().UTC(), time.Now().UTC()); err != nil {
					return err
				}
			}
			id, err := security.NewUUID()
			if err != nil {
				return err
			}
			next := m.state
			if next.Generation >= 1<<63-1 {
				return errors.New("Core generation exhausted")
			}
			next.Generation++
			next.InstanceID = id
			next.Process = current
			if err = m.save(next); err != nil {
				return err
			}
		}
		if err := m.finishBoundary(m.state.Boundary, true); err != nil {
			return err
		}
		next := m.state
		next.Process, next.Candidate, next.Phase = current, nil, "stable"
		if err := m.save(next); err != nil {
			return err
		}
		return m.prune()
	}
	if current != nil {
		if _, err := m.captureBoundary(ctx, true); err != nil {
			return err
		}
		if err := stopCoreProcess(ctx, *current, m.binary); err != nil {
			return err
		}
	} else if m.state.Phase == "stable" && m.state.Process != nil {
		if _, err := m.captureBoundary(ctx, true); err != nil {
			return err
		}
	}
	if err := m.finishBoundary(m.state.Boundary, false); err != nil {
		return err
	}
	if err := m.restore(ctx); err != nil {
		return m.fail(protocol.ErrorCoreRecoveryFailed)
	}
	return m.prune()
}

func (m *CoreManager) preflight(bindings []string) error {
	ownedPorts := map[string]bool{}
	if m.state.Current != nil && m.state.Process != nil {
		for _, address := range m.state.Current.Listeners {
			_, port, _ := net.SplitHostPort(address)
			ownedPorts[port] = true
		}
	}
	for _, address := range bindings {
		_, port, _ := net.SplitHostPort(address)
		if ownedPorts[port] {
			continue
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		if err := listener.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (m *CoreManager) Apply(ctx context.Context, epoch string, revision uint64, digest string, desired protocol.DesiredState) error {
	return m.apply(ctx, epoch, revision, digest, desired, true)
}

func (m *CoreManager) apply(ctx context.Context, epoch string, revision uint64, digest string, desired protocol.DesiredState, confirmedPolicy bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	calculated, err := protocol.DigestDesiredState(desired)
	if err != nil || calculated != digest || !security.IsUUIDv4(epoch) || revision == 0 {
		return m.fail(protocol.ErrorCoreConfigInvalid)
	}
	rendered, err := m.renderWithPermissionFloor(desired, !confirmedPolicy)
	if err != nil || len(rendered.Bytes) > 1<<20 {
		return m.fail(protocol.ErrorCoreConfigInvalid)
	}
	hash := sha256.Sum256(rendered.Bytes)
	effectiveRestrictions := desired.QuotaRestrictions
	if !confirmedPolicy && m.state.PermissionDigest != "" {
		floor, floorErr := m.readDesired(&coreConfigRecord{Digest: m.state.PermissionDigest})
		if floorErr != nil {
			return floorErr
		}
		effectiveRestrictions, err = protocol.MergeQuotaRestrictions(floor.QuotaRestrictions, desired.QuotaRestrictions)
		if err != nil {
			return m.fail(protocol.ErrorCoreConfigInvalid)
		}
	}
	restrictionDigest, err := effectiveRestrictions.Digest()
	if err != nil {
		return m.fail(protocol.ErrorCoreConfigInvalid)
	}
	if err := m.persistDesired(digest, desired); err != nil {
		return m.fail(protocol.ErrorCoreStorageFailed)
	}
	candidate := &coreConfigRecord{AuthorityEpoch: epoch, Revision: revision, Digest: digest, ConfigHash: hex.EncodeToString(hash[:]), Mappings: rendered.Mappings, Listeners: rendered.Listeners, HasSource: true, PeriodPolicies: desired.PeriodPolicies, RestrictionDigest: restrictionDigest}
	if !confirmedPolicy && m.state.PermissionDigest != "" {
		floor, err := m.readDesired(&coreConfigRecord{Digest: m.state.PermissionDigest})
		if err != nil {
			return err
		}
		candidate.PeriodPolicies = floor.PeriodPolicies
	}
	if current := m.state.Current; current != nil && current.AuthorityEpoch == epoch && current.Revision == revision && current.Digest == digest && current.ConfigHash == candidate.ConfigHash {
		if !current.HasSource || confirmedPolicy && m.state.PermissionDigest != digest {
			next := m.state
			next.Current = candidate
			if confirmedPolicy {
				next.PermissionDigest = digest
			}
			return m.save(next)
		}
		return nil
	}
	if _, err := os.Lstat(m.configPath(candidate)); errors.Is(err, os.ErrNotExist) {
		if err := storage.WritePrivateAtomic(m.configPath(candidate), rendered.Bytes); err != nil {
			return m.fail(protocol.ErrorCoreStorageFailed)
		}
	} else if err != nil || m.verifyConfig(candidate) != nil {
		return m.fail(protocol.ErrorCoreStorageFailed)
	}
	next := m.state
	next.Candidate, next.Phase = candidate, "validating"
	if err := m.save(next); err != nil {
		return err
	}
	if err := m.testConfig(ctx, candidate); err != nil {
		return m.fail(protocol.ErrorCoreConfigInvalid)
	}
	if err := m.preflight(rendered.Bindings); err != nil {
		return m.fail(protocol.ErrorCorePortConflict)
	}
	next = m.state
	next.Phase = "prepared"
	if err := m.save(next); err != nil {
		return err
	}
	previous := m.state.Current
	if previous != nil && previous.ConfigHash == candidate.ConfigHash {
		before, _ := json.Marshal(previous.PeriodPolicies)
		after, _ := json.Marshal(candidate.PeriodPolicies)
		if !bytes.Equal(before, after) {
			boundary, err := m.captureBoundary(ctx, false)
			if err != nil {
				return err
			}
			if err = m.finishBoundary(boundary, true); err != nil {
				return err
			}
			next = m.state
		}
		next.Current, next.Candidate, next.Phase, next.LastError = candidate, nil, "stable", ""
		if confirmedPolicy {
			next.PermissionDigest = digest
		}
		if err := m.save(next); err != nil {
			return err
		}
		return m.prune()
	}
	boundary, err := m.captureBoundary(ctx, false)
	if err != nil {
		return m.fail(protocol.ErrorCoreSampleFailed)
	}
	next = m.state
	next.Phase = "stopping"
	if err := m.save(next); err != nil {
		return err
	}
	if next.Process != nil {
		if err := stopCoreProcess(ctx, *next.Process, m.binary); err != nil {
			return m.rollback(ctx, protocol.ErrorCoreStartFailed)
		}
	}
	if err := m.finishBoundary(boundary, false); err != nil {
		return m.rollback(ctx, protocol.ErrorCoreStorageFailed)
	}
	process, err := m.start(ctx, candidate, "starting")
	if err != nil {
		return m.rollback(ctx, protocol.ErrorCoreStartFailed)
	}
	if err := m.health(ctx, process, candidate, true); err != nil {
		return m.rollback(ctx, protocol.ErrorCoreHealthFailed)
	}
	next = m.state
	next.Current, next.LKG, next.Candidate, next.Phase, next.LastError = candidate, previous, nil, "stable", ""
	if confirmedPolicy {
		next.PermissionDigest = digest
	}
	if next.LKG == nil {
		next.LKG = candidate
	}
	if err := m.save(next); err != nil {
		return err
	}
	return m.prune()
}

func (m *CoreManager) testConfig(ctx context.Context, record *coreConfigRecord) error {
	if err := m.verifyConfig(record); err != nil {
		return err
	}
	checkContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return runCoreConfigTest(checkContext, m.binary, m.configPath(record))
}

func runCoreConfigTest(ctx context.Context, binary, configPath string) error {
	return exec.CommandContext(ctx, binary, "run", "-test", "-config", configPath).Run()
}

func (m *CoreManager) rollback(ctx context.Context, code string) error {
	// Once the old Core has stopped, finish a bounded rollback even if the
	// request was cancelled; otherwise cancellation itself would cause downtime.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	processes, err := m.ownedProcesses()
	if err != nil {
		return err
	}
	candidateFound := false
	for _, process := range processes {
		if m.state.Candidate != nil && process.ConfigPath == m.configPath(m.state.Candidate) {
			candidateFound = true
		}
		if err := m.captureCandidate(cleanup, process); err != nil {
			return err
		}
		if err := stopCoreProcess(cleanup, process, m.binary); err != nil {
			return err
		}
		if err := m.finishBoundary(m.state.Boundary, false); err != nil {
			return err
		}
	}
	if err := m.captureLostCandidate(cleanup, candidateFound); err != nil {
		return err
	}
	if m.state.Current != nil {
		if err := m.restore(cleanup); err != nil {
			return m.fail(protocol.ErrorCoreRollbackFailed)
		}
	}
	return m.fail(code)
}

func (m *CoreManager) fail(code string) error {
	next := m.state
	next.LastError, next.Candidate = code, nil
	if code != protocol.ErrorCoreRollbackFailed && code != protocol.ErrorCoreRecoveryFailed {
		next.Phase = "stable"
	}
	if err := m.save(next); err != nil {
		return err
	}
	if err := m.prune(); err != nil {
		return err
	}
	return errors.New(code)
}

func (m *CoreManager) Status() CoreApplyStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := CoreApplyStatus{Generation: m.state.Generation, InstanceID: m.state.InstanceID, Error: m.state.LastError}
	if current := m.state.Current; current != nil {
		status.AuthorityEpoch, status.Revision, status.Digest = current.AuthorityEpoch, current.Revision, current.Digest
		status.RestrictionDigest, status.RestrictionAppliedAt = current.RestrictionDigest, current.RestrictionAppliedAt
		if m.state.Process != nil {
			actual, err := inspectCoreProcess(m.state.Process.PID, m.binary)
			status.Running = err == nil && actual == *m.state.Process && actual.ConfigPath == m.configPath(current)
		}
	}
	return status
}

func (m *CoreManager) prune() error {
	protected := map[string]bool{}
	if m.state.PermissionDigest != "" {
		protected[filepath.Base(m.desiredPath(m.state.PermissionDigest))] = true
	}
	for _, record := range []*coreConfigRecord{m.state.Current, m.state.LKG, m.state.Candidate} {
		if record != nil {
			protected[filepath.Base(m.configPath(record))] = true
			if record.HasSource {
				protected[filepath.Base(m.desiredPath(record.Digest))] = true
			}
		}
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") && !entry.IsDir() {
			base, token, ok := strings.Cut(strings.TrimSuffix(name, ".tmp"), ".json.")
			if ok && security.IsValidToken(token, 16) && (base == "state" || strings.HasPrefix(base, "config-") && security.IsValidDigest(strings.TrimPrefix(base, "config-")) || strings.HasPrefix(base, "desired-") && security.IsValidDigest(strings.TrimPrefix(base, "desired-"))) {
				if err := os.Remove(filepath.Join(m.dir, name)); err != nil {
					return err
				}
			}
		}
		if strings.HasPrefix(name, "config-") && strings.HasSuffix(name, ".json") && security.IsValidDigest(strings.TrimSuffix(strings.TrimPrefix(name, "config-"), ".json")) && !protected[name] && !entry.IsDir() {
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil {
				return err
			}
		}
		if strings.HasPrefix(name, "desired-") && strings.HasSuffix(name, ".json") && security.IsValidDigest(strings.TrimSuffix(strings.TrimPrefix(name, "desired-"), ".json")) && !protected[name] && !entry.IsDir() {
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
