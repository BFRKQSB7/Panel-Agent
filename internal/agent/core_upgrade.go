package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/install"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const (
	coreUpgradeJournalName = "upgrade.json"
	coreUpgradeDirName     = "upgrades"
	coreUpgradeHeadroom    = 32 << 20
)

type coreUpgradeJournal struct {
	Version        int    `json:"version"`
	IntentID       string `json:"intent_id"`
	Phase          string `json:"phase"`
	BinaryPath     string `json:"binary_path"`
	CandidatePath  string `json:"candidate_path"`
	BackupPath     string `json:"backup_path"`
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	CreatedAt      string `json:"created_at"`
}

type CoreUpgradeOutcome struct {
	ActualVersion string
	Generation    uint64
	RolledBack    bool
}

type CoreUpgradeError struct {
	Code       string
	RolledBack bool
	Err        error
}

func (e *CoreUpgradeError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *CoreUpgradeError) Unwrap() error { return e.Err }

func recoverPersistedUpgrade(dir, binary string) error {
	path := filepath.Join(dir, coreUpgradeJournalName)
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(contents) > 16<<10 {
		return errors.New("Core upgrade journal exceeds its size limit")
	}
	var journal coreUpgradeJournal
	if err := json.Unmarshal(contents, &journal); err != nil || journal.Version != 1 || !security.IsUUIDv4(journal.IntentID) || journal.BinaryPath != binary || !coreVersionSupported(journal.CurrentVersion) || !coreVersionSupported(journal.TargetVersion) {
		return errors.New("Core upgrade journal is invalid")
	}
	if journal.Phase != "prepared" && journal.Phase != "stopped" && journal.Phase != "swapped" && journal.Phase != "healthy" && journal.Phase != "rollback" && journal.Phase != "committed" {
		return errors.New("Core upgrade journal phase is invalid")
	}
	if !safeUpgradePath(dir, journal.CandidatePath) || !safeUpgradePath(dir, journal.BackupPath) {
		return errors.New("Core upgrade journal path is unsafe")
	}
	if journal.Phase == "healthy" || journal.Phase == "committed" {
		if err := removeIfRegular(journal.BackupPath); err != nil {
			return err
		}
		if err := removeUpgradeJournal(path); err != nil {
			return err
		}
		return nil
	}
	if journal.Phase == "prepared" {
		if err := removeIfRegular(journal.CandidatePath); err != nil {
			return err
		}
		return removeUpgradeJournal(path)
	}
	backupInfo, backupErr := os.Lstat(journal.BackupPath)
	if errors.Is(backupErr, os.ErrNotExist) {
		if journal.Phase == "stopped" {
			if err := removeIfRegular(journal.CandidatePath); err != nil {
				return err
			}
			return removeUpgradeJournal(path)
		}
		if journal.Phase == "rollback" {
			if currentVersion, versionErr := readCoreVersionWithTimeout(binary); versionErr == nil && currentVersion == journal.CurrentVersion {
				if err := removeIfRegular(journal.CandidatePath); err != nil {
					return err
				}
				return removeUpgradeJournal(path)
			}
		}
		return errors.New("Core upgrade journal lost its rollback binary")
	}
	if backupErr != nil {
		return backupErr
	}
	if backupInfo.Mode()&os.ModeSymlink != 0 || !backupInfo.Mode().IsRegular() {
		return errors.New("Core upgrade backup is unsafe")
	}
	if current, currentErr := os.Lstat(binary); currentErr == nil {
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return errors.New("current Core binary is unsafe during upgrade recovery")
		}
		if err := os.Remove(binary); err != nil {
			return err
		}
	} else if !errors.Is(currentErr, os.ErrNotExist) {
		return currentErr
	}
	if err := os.Rename(journal.BackupPath, binary); err != nil {
		return err
	}
	if err := removeIfRegular(journal.CandidatePath); err != nil {
		return err
	}
	return removeUpgradeJournal(path)
}

func readCoreOwnership(dataDir, binary string) (bool, error) {
	manifestPath := filepath.Join(dataDir, "ownership.json")
	info, err := os.Lstat(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 || info.Size() > install.MaxManifestBytes {
		return false, errors.New("Core ownership manifest is unsafe")
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, err
	}
	var manifest install.Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return false, errors.New("Core ownership manifest is invalid")
	}
	if manifest.Core.Owner == install.CoreExternal {
		return false, nil
	}
	if manifest.Core.Owner != install.CoreProject {
		return false, errors.New("Core ownership manifest has an unsupported owner")
	}
	if manifest.Core.Path != "" {
		ownedPath, err := filepath.EvalSymlinks(manifest.Core.Path)
		if err != nil || !sameCorePath(ownedPath, binary) {
			return false, errors.New("Core ownership manifest does not match the active binary")
		}
	}
	return true, nil
}

func sameCorePath(left, right string) bool {
	left, leftErr := filepath.Abs(left)
	right, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if os.PathSeparator == '\\' {
		return strings.EqualFold(left, right)
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func (m *CoreManager) CurrentVersion() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.binaryVersion
}

// UpgradeBinary swaps a verified candidate into the existing project-owned
// Core path. No second Core is started: the old process is sampled and
// stopped, the candidate is tested and started, and the old binary stays in a
// private rollback path until health succeeds.
func (m *CoreManager) UpgradeBinary(ctx context.Context, intentID, targetVersion, candidatePath, expectedSHA256 string) (CoreUpgradeOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.coreOwned {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: errors.New("external Core is not replaceable by Panel-Agent")}
	}
	if !security.IsUUIDv4(intentID) {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorInvalidIntent, Err: errors.New("intent id is invalid")}
	}
	if err := validateTargetCoreVersion(targetVersion); err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: err}
	}
	if !security.IsValidDigest(expectedSHA256) {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorChecksum, Err: errors.New("candidate checksum is invalid")}
	}
	if err := m.load(); err != nil {
		return CoreUpgradeOutcome{}, err
	}
	if m.state.Current == nil || m.state.Process == nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: errors.New("Core is not running")}
	}
	if err := validateUpgradeCandidate(m.dir, candidatePath, expectedSHA256); err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorChecksum, Err: err}
	}
	candidateVersion, err := readCoreVersionWithTimeout(candidatePath)
	if err != nil || candidateVersion != targetVersion {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: fmt.Errorf("candidate version is %q, want %q", candidateVersion, targetVersion)}
	}
	if candidateVersion == m.binaryVersion {
		return CoreUpgradeOutcome{ActualVersion: m.binaryVersion, Generation: m.state.Generation}, nil
	}
	if compareCoreVersions(candidateVersion, m.binaryVersion) < 0 {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: fmt.Errorf("不允许把 Core 从 %s 降级到 %s", m.binaryVersion, candidateVersion)}
	}
	oldInfo, err := os.Lstat(m.binary)
	if err != nil || oldInfo.Mode()&os.ModeSymlink != 0 || !oldInfo.Mode().IsRegular() {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorUnsupported, Err: errors.New("current Core binary is not a regular file")}
	}
	available, err := availableDiskBytes(filepath.Dir(m.binary))
	if err != nil || available < uint64(oldInfo.Size())+coreUpgradeHeadroom {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorLowDisk, Err: fmt.Errorf("available disk space is insufficient: %d", available)}
	}
	record := m.state.Current
	if err := m.testConfigWithBinary(ctx, candidatePath, record); err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorConfig, Err: err}
	}
	if err := m.preflight(record.Listeners); err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorConfig, Err: err}
	}
	boundary, err := m.captureBoundary(ctx, false)
	if err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorConfig, Err: err}
	}
	if err := m.finishBoundary(boundary, false); err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorConfig, Err: err}
	}
	backupPath := filepath.Join(m.dir, coreUpgradeDirName, intentID+".old")
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return CoreUpgradeOutcome{}, err
	}
	journal := coreUpgradeJournal{Version: 1, IntentID: intentID, Phase: "prepared", BinaryPath: m.binary, CandidatePath: candidatePath, BackupPath: backupPath, CurrentVersion: m.binaryVersion, TargetVersion: targetVersion, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	journalPath := filepath.Join(m.dir, coreUpgradeJournalName)
	if err := writeCoreUpgradeJournal(journalPath, journal); err != nil {
		return CoreUpgradeOutcome{}, err
	}
	if m.upgradeCheckpoint != nil {
		m.upgradeCheckpoint("prepared")
	}
	clearJournal := false
	defer func() {
		if clearJournal {
			_ = removeUpgradeJournal(journalPath)
		}
	}()
	next := m.state
	next.Phase = "stopping"
	if err := m.save(next); err != nil {
		return CoreUpgradeOutcome{}, err
	}
	if err := stopCoreProcess(ctx, *m.state.Process, m.binary); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorStart, err)
	}
	journal.Phase = "stopped"
	if err := writeCoreUpgradeJournal(journalPath, journal); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	if m.upgradeCheckpoint != nil {
		m.upgradeCheckpoint("stopped")
	}
	if err := os.Rename(m.binary, backupPath); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	if err := os.Rename(candidatePath, m.binary); err != nil {
		_ = os.Rename(backupPath, m.binary)
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	journal.Phase = "swapped"
	if err := writeCoreUpgradeJournal(journalPath, journal); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	if m.upgradeCheckpoint != nil {
		m.upgradeCheckpoint("swapped")
	}
	m.binaryVersion = candidateVersion
	process, err := m.start(ctx, record, "starting")
	if err == nil {
		err = m.health(ctx, process, record, true)
	}
	if err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorHealth, err)
	}
	journal.Phase = "healthy"
	if err := writeCoreUpgradeJournal(journalPath, journal); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	next = m.state
	next.BinaryVersion = candidateVersion
	next.Phase = "stable"
	next.LastError = ""
	if err := m.save(next); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	journal.Phase = "committed"
	if err := writeCoreUpgradeJournal(journalPath, journal); err != nil {
		return m.rollbackBinaryUpgrade(ctx, journal, journalPath, protocol.UpgradeErrorRollback, err)
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return CoreUpgradeOutcome{ActualVersion: candidateVersion, Generation: m.state.Generation}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: err}
	}
	clearJournal = true
	return CoreUpgradeOutcome{ActualVersion: candidateVersion, Generation: m.state.Generation}, nil
}

func (m *CoreManager) rollbackBinaryUpgrade(ctx context.Context, journal coreUpgradeJournal, journalPath, code string, cause error) (CoreUpgradeOutcome, error) {
	journal.Phase = "rollback"
	_ = writeCoreUpgradeJournal(journalPath, journal)
	backupInfo, backupErr := os.Lstat(journal.BackupPath)
	if errors.Is(backupErr, os.ErrNotExist) {
		// The failure happened before the old binary was moved, or the caller
		// already restored it after a failed rename. Never remove the only
		// known-good executable in this state.
		if m.state.Process != nil && m.health(context.WithoutCancel(ctx), *m.state.Process, m.state.Current, false) == nil {
			next := m.state
			next.Phase, next.LastError = "stable", code
			_ = m.save(next)
			_ = removeIfRegular(journal.CandidatePath)
			_ = removeUpgradeJournal(journalPath)
			return CoreUpgradeOutcome{ActualVersion: m.binaryVersion, Generation: m.state.Generation}, &CoreUpgradeError{Code: code, Err: cause}
		}
		process, startErr := m.start(context.WithoutCancel(ctx), m.state.Current, "restoring")
		if startErr == nil {
			startErr = m.health(context.WithoutCancel(ctx), process, m.state.Current, true)
		}
		if startErr == nil {
			next := m.state
			next.Phase, next.LastError = "stable", code
			_ = m.save(next)
			_ = removeIfRegular(journal.CandidatePath)
			_ = removeUpgradeJournal(journalPath)
			return CoreUpgradeOutcome{ActualVersion: m.binaryVersion, Generation: m.state.Generation}, &CoreUpgradeError{Code: code, Err: cause}
		}
		return CoreUpgradeOutcome{RolledBack: false}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: errors.Join(cause, startErr)}
	}
	if backupErr != nil || backupInfo.Mode()&os.ModeSymlink != 0 || !backupInfo.Mode().IsRegular() {
		return CoreUpgradeOutcome{RolledBack: false}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: errors.Join(cause, errors.New("Core upgrade backup is unsafe"))}
	}
	if m.state.Process != nil {
		_ = stopCoreProcess(context.WithoutCancel(ctx), *m.state.Process, m.binary)
		m.state.Process = nil
	}
	if err := os.Remove(m.binary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return CoreUpgradeOutcome{RolledBack: false}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: errors.Join(cause, err)}
	}
	if err := os.Rename(journal.BackupPath, m.binary); err != nil {
		return CoreUpgradeOutcome{RolledBack: false}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: errors.Join(cause, err)}
	}
	m.binaryVersion = journal.CurrentVersion
	process, startErr := m.start(context.WithoutCancel(ctx), m.state.Current, "restoring")
	if startErr == nil {
		startErr = m.health(context.WithoutCancel(ctx), process, m.state.Current, true)
	}
	if startErr != nil {
		next := m.state
		next.Phase = "restoring"
		next.LastError = protocol.ErrorCoreRollbackFailed
		_ = m.save(next)
		return CoreUpgradeOutcome{RolledBack: false}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, Err: errors.Join(cause, startErr)}
	}
	next := m.state
	next.BinaryVersion = journal.CurrentVersion
	next.Phase = "stable"
	next.LastError = code
	if saveErr := m.save(next); saveErr != nil {
		return CoreUpgradeOutcome{RolledBack: true, Generation: m.state.Generation}, &CoreUpgradeError{Code: protocol.UpgradeErrorRollback, RolledBack: true, Err: errors.Join(cause, saveErr)}
	}
	_ = removeIfRegular(journal.CandidatePath)
	_ = removeUpgradeJournal(journalPath)
	return CoreUpgradeOutcome{ActualVersion: journal.CurrentVersion, Generation: m.state.Generation, RolledBack: true}, &CoreUpgradeError{Code: code, RolledBack: true, Err: cause}
}

func (m *CoreManager) testConfigWithBinary(ctx context.Context, binary string, record *coreConfigRecord) error {
	if err := m.verifyConfig(record); err != nil {
		return err
	}
	checkContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return runCoreConfigTest(checkContext, binary, m.configPath(record))
}

func validateUpgradeCandidate(root, path, expected string) error {
	if !safeUpgradePath(root, path) {
		return errors.New("upgrade candidate path is unsafe")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > protocol.MaxUpgradePackageBytes {
		return errors.New("upgrade candidate is not a bounded regular file")
	}
	if !strings.EqualFold(candidateDigest(path), expected) {
		return errors.New("upgrade candidate checksum mismatch")
	}
	return nil
}

func safeUpgradePath(root, path string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	return strings.HasPrefix(relative, coreUpgradeDirName+string(os.PathSeparator)) && !strings.Contains(relative, ".."+string(os.PathSeparator))
}

func writeCoreUpgradeJournal(path string, journal coreUpgradeJournal) error {
	contents, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return storage.WritePrivateAtomic(path, contents)
}

func removeUpgradeJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeIfRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("upgrade cleanup path is unsafe")
	}
	return os.Remove(path)
}

func readCoreVersionWithTimeout(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return readCoreVersion(ctx, binary)
}
