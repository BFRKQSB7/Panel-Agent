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
	if err := checkPanelVersion(ctx, packagePath, options.TargetVersion); err != nil {
		return SelfUpdateResult{}, err
	}
	lock, err := storage.AcquireProcessLock(dataDir)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	defer lock.Close()
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
	backupToken, err := security.NewToken(16)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	backupPath := filepath.Join(backupDir, "panel-update-"+backupToken+".json")
	if err := writePrivateFile(backupPath, contents); err != nil {
		return SelfUpdateResult{}, err
	}
	preflightDir, err := stagePanelPreflight(dataDir, snapshot.Database, snapshot.MasterKey)
	if err != nil {
		return SelfUpdateResult{}, fmt.Errorf("准备 Panel 迁移预检：%w", err)
	}
	preflightDone := false
	defer func() {
		if !preflightDone {
			_ = os.RemoveAll(preflightDir)
		}
	}()
	if err := checkPanelData(ctx, packagePath, preflightDir); err != nil {
		return SelfUpdateResult{}, err
	}
	if err := os.RemoveAll(preflightDir); err != nil {
		return SelfUpdateResult{}, fmt.Errorf("清理 Panel 迁移预检：%w", err)
	}
	preflightDone = true
	updateToken, err := security.NewToken(16)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	stagedPath := executable + ".update-" + updateToken
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
	if err := copyPrivateFile(stagedPath, packageBytes); err != nil {
		return SelfUpdateResult{}, err
	}
	if err := os.Rename(executable, previousPath); err != nil {
		_ = os.Remove(stagedPath)
		return SelfUpdateResult{}, fmt.Errorf("切换 Panel 旧版本：%w", err)
	}
	if err := os.Rename(stagedPath, executable); err != nil {
		_ = os.Remove(stagedPath)
		if rollbackErr := rollbackPanelExecutable(executable, previousPath); rollbackErr != nil {
			return SelfUpdateResult{}, fmt.Errorf("切换 Panel 候选版本且回滚失败：%w", errors.Join(err, rollbackErr))
		}
		return SelfUpdateResult{}, fmt.Errorf("切换 Panel 候选版本失败，已回滚：%w", err)
	}
	if err := checkPanelVersion(ctx, executable, options.TargetVersion); err != nil {
		if rollbackErr := rollbackPanelExecutable(executable, previousPath); rollbackErr != nil {
			return SelfUpdateResult{}, fmt.Errorf("候选 Panel 健康检查失败且回滚失败：%w", errors.Join(err, rollbackErr))
		}
		return SelfUpdateResult{}, fmt.Errorf("候选 Panel 健康检查失败，已回滚：%w", err)
	}
	rollbackPath := ""
	if err := os.Remove(previousPath); err != nil {
		// Windows keeps the image file of this updater process open even after
		// its path has been renamed. A healthy candidate is still a successful
		// update; retain the rollback file for cleanup by the next invocation.
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
	if !strings.Contains(string(output), "Panel-Agent "+expected) {
		return fmt.Errorf("候选 Panel 版本不匹配：%s", strings.TrimSpace(string(output)))
	}
	return nil
}

func stagePanelPreflight(dataDir string, database, masterKey []byte) (string, error) {
	stage, err := os.MkdirTemp(dataDir, ".panel-update-check-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	if err := writePrivateFile(filepath.Join(stage, storage.DatabaseFileName), database); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	if err := writePrivateFile(filepath.Join(stage, "master.key"), masterKey); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	return stage, nil
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
		if err := os.Remove(executable); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(previousPath, executable)
}
