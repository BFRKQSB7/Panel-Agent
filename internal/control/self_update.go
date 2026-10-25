package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/backup"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

var panelVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type SelfUpdateOptions struct {
	DataDir        string
	PackagePath    string
	PackageSHA256  string
	TargetVersion  string
	BackupPassword string
	ExecutablePath string
}

type SelfUpdateResult struct {
	BackupPath   string
	RollbackPath string
	Version      string
}

// UpdateSelf performs a deliberately local Panel update. It refuses to run
// beside the live Panel lock, writes a D16 complete backup first, verifies the
// candidate through the fixed version subcommand, and keeps a same-directory
// rollback copy until the candidate passes that health check.
func UpdateSelf(ctx context.Context, options SelfUpdateOptions) (SelfUpdateResult, error) {
	return updateSelf(ctx, options, nil)
}

// checkpoint is used only by subprocess crash tests, never by a CLI option.
func updateSelf(ctx context.Context, options SelfUpdateOptions, checkpoint func(string)) (result SelfUpdateResult, err error) {
	if !panelVersionPattern.MatchString(options.TargetVersion) || !security.IsValidDigest(options.PackageSHA256) || strings.TrimSpace(options.PackagePath) == "" {
		return SelfUpdateResult{}, errors.New("Panel 更新参数无效")
	}
	dataDir, err := storage.PrepareDataDir(options.DataDir)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	executable, err := filepath.Abs(options.ExecutablePath)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	packagePath, err := filepath.Abs(options.PackagePath)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	packageInfo, err := os.Lstat(packagePath)
	if err != nil || packageInfo.Mode()&os.ModeSymlink != 0 || !packageInfo.Mode().IsRegular() || packageInfo.Size() == 0 || packageInfo.Size() > protocol.MaxUpgradePackageBytes {
		return SelfUpdateResult{}, errors.New("Panel 候选文件不安全或大小越界")
	}
	packageBytes, err := os.ReadFile(packagePath)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	digest := sha256.Sum256(packageBytes)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), options.PackageSHA256) {
		return SelfUpdateResult{}, errors.New("Panel 候选文件 SHA-256 不匹配")
	}
	updateToken, err := security.NewToken(16)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	stagedPath := executable + ".update-" + updateToken
	if err := copyPrivateFile(stagedPath, packageBytes); err != nil {
		return SelfUpdateResult{}, err
	}
	defer func() {
		if _, e := os.Lstat(filepath.Join(dataDir, panelSelfUpdateJournalName)); errors.Is(e, os.ErrNotExist) {
			os.Remove(stagedPath)
		}
	}()
	if err := checkPanelVersion(ctx, stagedPath, options.TargetVersion); err != nil {
		return SelfUpdateResult{}, err
	}
	lock, err := storage.AcquireProcessLock(dataDir)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	defer lock.Close()
	if rolledBack, err := recoverPanelSelfUpdate(dataDir, executable); err != nil {
		return SelfUpdateResult{}, err
	} else if rolledBack {
		return SelfUpdateResult{}, errors.New("中断的 Panel 更新已回滚，请重新启动更新命令")
	}
	store, err := storage.Open(dataDir)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	snapshot, err := store.Snapshot(ctx)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return SelfUpdateResult{}, err
	}
	contents, _, err := backup.Create(backup.Input{Database: snapshot.Database, MasterKey: snapshot.MasterKey, SchemaVersion: snapshot.SchemaVersion, InstallationID: snapshot.InstallationID, Extensions: snapshot.Extensions, Password: options.BackupPassword, CreatedAt: time.Now().UTC()})
	if err != nil {
		return SelfUpdateResult{}, fmt.Errorf("生成 Panel 更新前备份：%w", err)
	}
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return SelfUpdateResult{}, err
	}
	if err := storage.ChownLike(backupDir, dataDir); err != nil {
		return SelfUpdateResult{}, err
	}
	backupToken, err := security.NewToken(16)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	backupPath := filepath.Join(backupDir, "panel-update-"+backupToken+".json")
	if err := writePrivateFile(backupPath, contents); err != nil {
		return SelfUpdateResult{}, err
	}
	verified, err := backup.Open(contents, options.BackupPassword)
	if err != nil {
		return SelfUpdateResult{}, fmt.Errorf("验证 Panel 更新前备份：%w", err)
	}
	preflight, err := backup.Stage(ctx, dataDir, verified, snapshot.InstallationID)
	if err != nil {
		return SelfUpdateResult{}, fmt.Errorf("准备 Panel 迁移预检：%w", err)
	}
	preflightDir := preflight.Path
	defer func() {
		// A failed recovery needs its full staged generation on the next start.
		if _, e := os.Lstat(filepath.Join(dataDir, panelSelfUpdateJournalName)); errors.Is(e, os.ErrNotExist) {
			_ = os.RemoveAll(preflightDir)
		}
	}()
	if err := checkPanelData(ctx, stagedPath, preflightDir); err != nil {
		return SelfUpdateResult{}, err
	}
	previousPath := executable + ".previous"
	if info, err := os.Lstat(previousPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return SelfUpdateResult{}, errors.New("已有不安全的 Panel rollback 路径，拒绝覆盖")
		}
		if err := os.Remove(previousPath); err != nil {
			return SelfUpdateResult{}, fmt.Errorf("已有 Panel rollback 文件仍未释放：%w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return SelfUpdateResult{}, err
	}
	journal := panelSelfUpdateJournal{Version: 1, Executable: executable, Previous: previousPath, Staged: stagedPath, DataStage: preflightDir, OriginalSHA: panelFileDigest(executable), CandidateSHA: options.PackageSHA256}
	originalInfo, err := os.Lstat(executable)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	if err := storage.ChownLike(stagedPath, executable); err != nil {
		return SelfUpdateResult{}, err
	}
	if err := os.Chmod(stagedPath, originalInfo.Mode().Perm()); err != nil {
		return SelfUpdateResult{}, err
	}
	if !security.IsValidDigest(journal.OriginalSHA) {
		os.Remove(stagedPath)
		return SelfUpdateResult{}, errors.New("无法验证原 Panel 程序")
	}
	if runtime.GOOS != "windows" {
		original, readErr := os.ReadFile(executable)
		if readErr != nil {
			os.Remove(stagedPath)
			return SelfUpdateResult{}, readErr
		}
		if err := copyPrivateFile(previousPath, original); err != nil {
			os.Remove(stagedPath)
			os.Remove(previousPath)
			return SelfUpdateResult{}, err
		}
		if err := storage.ChownLike(previousPath, executable); err != nil {
			return SelfUpdateResult{}, err
		}
		if err := os.Chmod(previousPath, originalInfo.Mode().Perm()); err != nil {
			return SelfUpdateResult{}, err
		}
	}
	if err := writePanelSelfUpdateJournal(dataDir, journal); err != nil {
		os.Remove(stagedPath)
		if runtime.GOOS != "windows" {
			os.Remove(previousPath)
		}
		return SelfUpdateResult{}, err
	}
	defer func() {
		if err != nil {
			_, recoveryErr := recoverPanelSelfUpdate(dataDir, executable)
			err = errors.Join(err, recoveryErr)
		}
	}()
	if runtime.GOOS == "windows" {
		if err := os.Rename(executable, previousPath); err != nil {
			return SelfUpdateResult{}, fmt.Errorf("切换 Panel 旧版本：%w", err)
		}
	}
	if err := os.Rename(stagedPath, executable); err != nil {
		return SelfUpdateResult{}, fmt.Errorf("切换 Panel 候选版本失败：%w", err)
	}
	if checkpoint != nil {
		checkpoint("binary-swapped")
	}
	// Publish the already migrated, verified generation together with the
	// candidate binary. D16 retains the entire old DB/key/extension generation.
	if err := backup.Swap(dataDir, preflightDir); err != nil {
		return SelfUpdateResult{}, fmt.Errorf("切换 Panel 数据代失败：%w", err)
	}
	if checkpoint != nil {
		checkpoint("data-swapped")
	}
	if err := checkPanelVersion(ctx, executable, options.TargetVersion); err != nil {
		return SelfUpdateResult{}, fmt.Errorf("候选 Panel 健康检查失败：%w", err)
	}
	journal.Committed = true
	if err := writePanelSelfUpdateJournal(dataDir, journal); err != nil {
		return SelfUpdateResult{}, err
	}
	if checkpoint != nil {
		checkpoint("committed")
	}
	if _, err := recoverPanelSelfUpdate(dataDir, executable); err != nil {
		return SelfUpdateResult{}, err
	}
	rollbackPath := ""
	if _, err := os.Stat(previousPath); err == nil {
		rollbackPath = previousPath
	}
	return SelfUpdateResult{BackupPath: backupPath, RollbackPath: rollbackPath, Version: options.TargetVersion}, nil
}

func checkPanelVersion(ctx context.Context, binary, expected string) error {
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(checkContext, binary, "version").CombinedOutput()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "Panel-Agent "+expected {
		return fmt.Errorf("候选 Panel 版本不匹配：%s", strings.TrimSpace(string(output)))
	}
	return nil
}

func checkPanelData(ctx context.Context, binary, dataDir string) error {
	checkContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(checkContext, binary, "check", "--data-dir", dataDir).CombinedOutput(); err != nil {
		return fmt.Errorf("候选 Panel 数据迁移预检失败：%w", sanitizePanelCheckOutput(output))
	}
	return nil
}

func sanitizePanelCheckOutput(output []byte) error {
	message := strings.TrimSpace(string(output))
	if len([]byte(message)) > protocol.MaxUpgradeDetailBytes {
		message = string([]byte(message)[:protocol.MaxUpgradeDetailBytes])
	}
	if message == "" {
		return errors.New("候选 Panel check 返回失败")
	}
	return errors.New(message)
}

func copyPrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := storage.ChownLike(path, filepath.Dir(path)); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func rollbackPanelExecutable(executable, previousPath string) error {
	if info, err := os.Lstat(executable); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Panel 候选文件回滚路径不安全")
		}
		if runtime.GOOS == "windows" {
			if err := os.Remove(executable); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(previousPath, executable)
}
