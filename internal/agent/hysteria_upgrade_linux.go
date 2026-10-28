package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/install"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const hysteriaUpgradeName = "hysteria-upgrade.json"

type hysteriaUpgradeJournal struct {
	Version                int
	Binary, Staged, Backup string
	OriginalSHA, TargetSHA string
	ConfigDigest           string
	Committed              bool
}

func syncHysteriaDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeHysteriaUpgrade(dir string, j hysteriaUpgradeJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return storage.WritePrivateAtomic(filepath.Join(dir, hysteriaUpgradeName), data)
}

// Called only after owned orphan processes and their sources are stopped.
func recoverHysteriaUpgrade(dir, binary string) error {
	p := filepath.Join(dir, hysteriaUpgradeName)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return errors.New("hysteria: unsafe upgrade journal")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var j hysteriaUpgradeJournal
	if json.Unmarshal(data, &j) != nil || j.Version != 1 || j.Binary != binary || j.Staged != binary+".candidate" || j.Backup != binary+".previous" || !security.IsValidDigest(j.OriginalSHA) || !security.IsValidDigest(j.TargetSHA) || !security.IsValidDigest(j.ConfigDigest) {
		return errors.New("hysteria: invalid upgrade journal paths")
	}
	if j.Committed {
		if candidateDigest(binary) != j.TargetSHA {
			return errors.New("hysteria: committed upgrade checksum mismatch")
		}
	} else if candidateDigest(binary) != j.OriginalSHA {
		if _, err := hysteria.VerifiedVersion(j.Backup, runtime.GOARCH); err != nil || candidateDigest(j.Backup) != j.OriginalSHA {
			return errors.New("hysteria: rollback asset unavailable")
		}
		if err := os.Rename(j.Backup, binary); err != nil {
			return err
		}
		if err := syncHysteriaDir(filepath.Dir(binary)); err != nil {
			return err
		}
	}
	if _, err := hysteria.VerifiedVersion(binary, runtime.GOARCH); err != nil {
		return err
	}
	if !j.Committed {
		p := filepath.Join(dir, "hysteria-state.json")
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var r hysteriaRecord
		if json.Unmarshal(data, &r) != nil || r.Digest != j.ConfigDigest {
			return errors.New("hysteria: upgrade configuration identity changed")
		}
		r.Pending = false
		encoded, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := storage.WritePrivateAtomic(p, encoded); err != nil {
			return err
		}
	}
	for p, expected := range map[string]string{j.Staged: j.TargetSHA, j.Backup: j.OriginalSHA} {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if candidateDigest(p) != expected {
			return errors.New("hysteria: refusing to remove altered upgrade file")
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, hysteriaUpgradeName)); err != nil {
		return err
	}
	return errors.Join(syncHysteriaDir(filepath.Dir(binary)), syncHysteriaDir(dir))
}

// Upgrade is local/manual and runs under the exclusive Agent lock. There is no
// network download or remote intent execution in the Hysteria upgrade path.
func (m *hysteriaManager) Upgrade(ctx context.Context, candidate, version, digest string) (result error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := hysteria.VerifiedVersion(m.binary, runtime.GOARCH)
	if err != nil {
		return err
	}
	target, err := hysteria.VerifiedVersion(candidate, runtime.GOARCH)
	if err != nil || target != version || candidateDigest(candidate) != digest {
		return errors.New("hysteria: candidate origin/version/checksum mismatch")
	}
	if compareCoreVersions(target, current) < 0 {
		return errors.New("hysteria: downgrade refused")
	}
	if candidateDigest(m.binary) == digest {
		return nil
	}
	if m.record.Pending || m.record.Digest == "" {
		return errors.New("hysteria: complete the pending configuration before upgrade")
	}
	if info, err := os.Lstat(filepath.Join(m.dir, "ownership.json")); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("hysteria: unsafe ownership manifest")
		}
		data, err := os.ReadFile(filepath.Join(m.dir, "ownership.json"))
		if err != nil {
			return err
		}
		var manifest install.Manifest
		if json.Unmarshal(data, &manifest) != nil || manifest.Hysteria == nil || manifest.Hysteria.Owner != install.CoreProject || manifest.Hysteria.Path != m.binary {
			return errors.New("hysteria: upgrade requires project ownership")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		relative, err := filepath.Rel(m.dir, m.binary)
		if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) > 3 && relative[:3] == ".."+string(os.PathSeparator) {
			return errors.New("hysteria: unowned external binary cannot be upgraded")
		}
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return err
	}
	available, err := availableDiskBytes(filepath.Dir(m.binary))
	if err != nil || available < uint64(info.Size())*2+coreUpgradeHeadroom {
		return errors.New("hysteria: insufficient upgrade space")
	}
	j := hysteriaUpgradeJournal{Version: 1, Binary: m.binary, Staged: m.binary + ".candidate", Backup: m.binary + ".previous", OriginalSHA: candidateDigest(m.binary), TargetSHA: digest, ConfigDigest: m.record.Digest}
	for _, p := range []string{j.Staged, j.Backup, filepath.Join(m.dir, hysteriaUpgradeName)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return errors.New("hysteria: unfinished upgrade files require recovery")
		}
	}
	if err := copyAgentProgram(candidate, j.Staged); err != nil {
		return err
	}
	defer func() {
		if _, err := os.Stat(filepath.Join(m.dir, hysteriaUpgradeName)); errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(j.Staged)
			_ = os.Remove(j.Backup)
		}
	}()
	if err := storage.ChownLike(j.Staged, m.binary); err != nil {
		return err
	}
	if err := copyAgentProgram(m.binary, j.Backup); err != nil {
		return err
	}
	if err := storage.ChownLike(j.Backup, m.binary); err != nil {
		return err
	}
	if candidateDigest(j.Staged) != j.TargetSHA || candidateDigest(j.Backup) != j.OriginalSHA {
		return errors.New("hysteria: staged/rollback checksum mismatch")
	}
	if err := syncHysteriaDir(filepath.Dir(m.binary)); err != nil {
		return err
	}
	if err := writeHysteriaUpgrade(m.dir, j); err != nil {
		return err
	}
	if m.upgradeCheckpoint != nil {
		m.upgradeCheckpoint("prepared")
	}
	original := m.record
	defer func() {
		if result == nil || j.Committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for id, s := range m.sessions {
			if err := s.stop(cleanup); err != nil && !s.source.state.Closed {
				result = errors.Join(result, err)
				return
			}
			delete(m.sessions, id)
			delete(m.hashes, id)
		}
		if err := recoverHysteriaSessions(cleanup, m.dir, m.book, m.binary); err != nil {
			result = errors.Join(result, err)
			return
		}
		if err := recoverHysteriaUpgrade(m.dir, m.binary); err != nil {
			result = errors.Join(result, err)
			return
		}
		m.version = current
		original.Generation = max(original.Generation, m.record.Generation)
		if err := m.save(original); err != nil {
			result = errors.Join(result, err)
			return
		}
		result = errors.Join(result, m.applyLocked(cleanup, original.Authority, original.Revision, original.Digest, original.Desired))
	}()
	for id, s := range m.sessions {
		if err := s.stop(ctx); err != nil && !s.source.state.Closed {
			return err
		}
		if err := s.source.retire(ctx); err != nil {
			return err
		}
		delete(m.sessions, id)
		delete(m.hashes, id)
	}
	if err := os.Rename(j.Staged, m.binary); err != nil {
		return err
	}
	if err := syncHysteriaDir(filepath.Dir(m.binary)); err != nil {
		return err
	}
	if m.upgradeCheckpoint != nil {
		m.upgradeCheckpoint("swapped")
	}
	m.version = target
	if err := m.applyLocked(ctx, original.Authority, original.Revision, original.Digest, original.Desired); err != nil {
		return err
	}
	j.Committed = true
	if err := writeHysteriaUpgrade(m.dir, j); err != nil {
		j.Committed = false
		return err
	}
	return recoverHysteriaUpgrade(m.dir, m.binary)
}

func (c *Client) UpgradeHysteria(ctx context.Context, candidate, version, digest string) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.hysteria == nil {
		return errors.New("Hysteria manager is not enabled")
	}
	return c.hysteria.Upgrade(ctx, candidate, version, digest)
}
