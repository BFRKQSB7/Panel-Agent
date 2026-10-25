package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/install"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const agentProgramJournalName = "agent.program-update.json"

type agentProgramJournal struct {
	Executable   string `json:"executable"`
	Staged       string `json:"staged"`
	OriginalSHA  string `json:"original_sha256"`
	CandidateSHA string `json:"candidate_sha256"`
	Committed    bool   `json:"committed"`
}

// CheckProgramData is read-only: V1 binary updates do not migrate identity or
// ledger formats. It may run while the updater holds the Agent process lock.
func CheckProgramData(ctx context.Context, dataDir string) error {
	s, err := openIdentityStore(dataDir)
	if err != nil {
		return err
	}
	identity, err := s.Load()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.dataDir, "agent.ledger.sqlite")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	d, err := ledger.Inspect(ctx, s.dataDir)
	if err != nil {
		return err
	}
	if d.Metadata.MachineID != identity.MachineID {
		return errors.New("Agent 身份与账本不匹配")
	}
	return nil
}

func checkAgentProgram(ctx context.Context, path, version, dataDir string) error {
	for _, args := range [][]string{{"version"}, {"check", "--data-dir", dataDir}} {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		output := &limitedCoreOutput{limit: 4096}
		command := exec.CommandContext(probe, path, args...)
		command.Stdout, command.Stderr = output, output
		err := command.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("Agent 候选检查失败：%w", err)
		}
		if args[0] == "version" && strings.TrimSpace(output.String()) != "Control-Agent "+version {
			return errors.New("Agent 候选版本不匹配")
		}
	}
	return nil
}

func copyAgentProgram(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, io.LimitReader(in, protocol.MaxUpgradePackageBytes+1))
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// UpdateProgram updates only the local Agent executable with the daemon
// stopped. The Core, credentials and durable ledger are never replaced.
func UpdateProgram(ctx context.Context, dataDir, executable, packagePath, version, digest string) error {
	return updateProgram(ctx, dataDir, executable, packagePath, version, digest, nil)
}

func updateProgram(ctx context.Context, dataDir, executable, packagePath, version, digest string, checkpoint func(string)) (err error) {
	if runtime.GOOS != "linux" {
		return errors.New("Agent 本机更新仅支持 Linux")
	}
	if _, ok := parseCoreVersion(version); !ok || compareCoreVersions(version, protocol.AgentVersion) < 0 || !security.IsValidDigest(digest) {
		return errors.New("Agent 更新参数无效")
	}
	s, err := openIdentityStore(dataDir)
	if err != nil {
		return err
	}
	dataDir = s.dataDir
	lock, err := storage.AcquireDataLock(dataDir, "agent.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	if rolledBack, e := recoverAgentProgram(dataDir, executable); e != nil {
		return e
	} else if rolledBack {
		return errors.New("Agent 更新已回滚，请重新启动更新命令")
	}
	if info, e := os.Lstat(packagePath); e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > protocol.MaxUpgradePackageBytes || !strings.EqualFold(candidateDigest(packagePath), digest) {
		return errors.New("Agent 候选文件或校验无效")
	}
	if contents, e := os.ReadFile(filepath.Join(dataDir, "ownership.json")); e == nil {
		var m install.Manifest
		if json.Unmarshal(contents, &m) != nil {
			return errors.New("Agent 所有权清单无效")
		}
		owned := false
		for _, p := range m.Paths {
			if m.DataDir == dataDir && p.Kind == "binary" && filepath.Base(p.Path) == "control-agent" && filepath.Clean(p.Path) == executable {
				owned = true
			}
		}
		if !owned {
			return errors.New("Agent 程序不属于当前安装")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err := CheckProgramData(ctx, dataDir); err != nil {
		return err
	}
	if err := checkAgentProgram(ctx, packagePath, version, dataDir); err != nil {
		return err
	}
	info, err := os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Size() > protocol.MaxUpgradePackageBytes {
		return errors.New("原 Agent 程序无效")
	}
	available, err := availableDiskBytes(filepath.Dir(executable))
	if err != nil || available < uint64(info.Size())+protocol.MaxUpgradePackageBytes+coreUpgradeHeadroom {
		return errors.New("Agent 更新空间不足")
	}
	previous := executable + ".previous"
	if _, e := os.Lstat(previous); !errors.Is(e, os.ErrNotExist) {
		return errors.New("Agent 回滚程序尚未清理，拒绝覆盖")
	}
	id, err := security.NewUUID()
	if err != nil {
		return err
	}
	staged := executable + ".update-" + id
	if err := copyAgentProgram(packagePath, staged); err != nil {
		os.Remove(staged)
		return err
	}
	defer func() {
		if _, e := os.Lstat(filepath.Join(dataDir, agentProgramJournalName)); errors.Is(e, os.ErrNotExist) {
			os.Remove(staged)
		}
	}()
	j := agentProgramJournal{Executable: executable, Staged: staged, OriginalSHA: candidateDigest(executable), CandidateSHA: strings.ToLower(digest)}
	if err := storage.ChownLike(staged, executable); err != nil {
		return err
	}
	if err := os.Chmod(staged, info.Mode().Perm()); err != nil {
		return err
	}
	if candidateDigest(staged) != j.CandidateSHA || !security.IsValidDigest(j.OriginalSHA) {
		return errors.New("Agent 暂存程序校验失败")
	}
	write := func() error {
		b, e := json.Marshal(j)
		if e != nil {
			return e
		}
		return storage.WritePrivateAtomic(filepath.Join(dataDir, agentProgramJournalName), b)
	}
	if err := copyAgentProgram(executable, previous); err != nil {
		os.Remove(previous)
		return err
	}
	if candidateDigest(previous) != j.OriginalSHA {
		os.Remove(previous)
		return errors.New("Agent 回滚程序校验失败")
	}
	if err := storage.ChownLike(previous, executable); err != nil {
		return err
	}
	if err := os.Chmod(previous, info.Mode().Perm()); err != nil {
		return err
	}
	if err := write(); err != nil {
		os.Remove(previous)
		return err
	}
	defer func() {
		if err != nil {
			_, e := recoverAgentProgram(dataDir, executable)
			err = errors.Join(err, e)
		}
	}()
	if err := os.Rename(staged, executable); err != nil {
		return err
	}
	if checkpoint != nil {
		checkpoint("swapped")
	}
	if err := checkAgentProgram(ctx, executable, version, dataDir); err != nil {
		return err
	}
	j.Committed = true
	if err := write(); err != nil {
		return err
	}
	if checkpoint != nil {
		checkpoint("committed")
	}
	_, err = recoverAgentProgram(dataDir, executable)
	return err
}

func recoverAgentProgram(dir, executable string) (bool, error) {
	path := filepath.Join(dir, agentProgramJournalName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return false, errors.New("Agent 更新 journal 不安全")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var j agentProgramJournal
	executable, err = filepath.Abs(executable)
	if err != nil {
		return false, err
	}
	if json.Unmarshal(b, &j) != nil || j.Executable != executable || filepath.Dir(j.Staged) != filepath.Dir(executable) || !strings.HasPrefix(j.Staged, executable+".update-") || !security.IsValidDigest(j.OriginalSHA) || !security.IsValidDigest(j.CandidateSHA) {
		return false, errors.New("Agent 更新 journal 无效")
	}
	previous := executable + ".previous"
	if j.Committed {
		if candidateDigest(executable) != j.CandidateSHA {
			return false, errors.New("Agent 已提交程序校验失败")
		}
	} else if candidateDigest(previous) == j.OriginalSHA {
		if err := os.Rename(previous, executable); err != nil {
			return false, err
		}
	} else if candidateDigest(executable) != j.OriginalSHA {
		return false, errors.New("Agent 原程序丢失，保留现场")
	}
	for _, p := range []string{previous, j.Staged} {
		if info, e := os.Lstat(p); e == nil {
			if !info.Mode().IsRegular() {
				return false, errors.New("Agent 更新清理路径不安全")
			}
			want := j.CandidateSHA
			if p == previous {
				want = j.OriginalSHA
			}
			if candidateDigest(p) != want {
				return false, errors.New("Agent 更新清理文件校验失败")
			}
			if e := os.Remove(p); e != nil {
				return false, e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return false, e
		}
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return !j.Committed, nil
}
