package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const officialXrayReleaseBaseURL = "https://github.com/XTLS/Xray-core/releases/download/v"

func (c *Client) syncUpgrades(ctx context.Context) error {
	if c.core == nil {
		return nil
	}
	requestID, err := security.NewUUID()
	if err != nil {
		return err
	}
	var response protocol.UpgradePullResponse
	if _, err := c.postJSON(ctx, "/api/v1/agent/upgrade-intents", protocol.UpgradePullRequest{Version: protocol.UpgradeProtocolVersion, RequestID: requestID, AuthorityEpoch: c.identity.AuthorityEpoch}, c.identity.MachineSecret, &response); err != nil {
		return err
	}
	if response.Version != protocol.UpgradeProtocolVersion || response.RequestID != requestID || response.AuthorityEpoch != c.identity.AuthorityEpoch {
		return errors.New("Panel returned an invalid upgrade intent response")
	}
	for _, intent := range response.Intents {
		if intent.Version != protocol.UpgradeProtocolVersion || intent.ID == "" || intent.MachineID != c.identity.MachineID || intent.AuthorityEpoch != c.identity.AuthorityEpoch || intent.State != protocol.UpgradeStateRunning {
			return errors.New("Panel returned an invalid upgrade intent")
		}
		outcome, upgradeErr := c.executeUpgradeIntent(ctx, intent)
		result := protocol.UpgradeResultRequest{Version: protocol.UpgradeProtocolVersion, IntentID: intent.ID, AuthorityEpoch: intent.AuthorityEpoch, TargetVersion: intent.TargetVersion, State: protocol.UpgradeStateSucceeded, ActualVersion: outcome.ActualVersion, RolledBack: outcome.RolledBack, CoreGeneration: outcome.Generation}
		if upgradeErr != nil {
			result.State = protocol.UpgradeStateFailed
			result.ResultCode, result.ResultDetail = upgradeErrorDetails(upgradeErr)
		}
		resultID, idErr := security.NewUUID()
		if idErr != nil {
			return idErr
		}
		result.RequestID = resultID
		var accepted protocol.UpgradeResultResponse
		if _, err := c.postJSON(ctx, "/api/v1/agent/upgrade-results", result, c.identity.MachineSecret, &accepted); err != nil {
			return errors.Join(upgradeErr, err)
		}
		if accepted.Version != protocol.UpgradeProtocolVersion || accepted.RequestID != resultID || !accepted.Accepted {
			return errors.Join(upgradeErr, errors.New("Panel rejected the upgrade result"))
		}
		if upgradeErr != nil {
			return upgradeErr
		}
	}
	return nil
}

func (c *Client) executeUpgradeIntent(ctx context.Context, intent protocol.UpgradeIntent) (CoreUpgradeOutcome, error) {
	packagePath, err := c.downloadUpgradePackage(ctx, intent)
	if err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorDownload, Err: err}
	}
	outcome, upgradeErr := c.core.UpgradeBinary(ctx, intent.ID, intent.TargetVersion, packagePath, candidateDigest(packagePath))
	if upgradeErr == nil {
		_ = os.RemoveAll(filepath.Dir(packagePath))
	} else {
		var typed *CoreUpgradeError
		if errors.As(upgradeErr, &typed) && typed.RolledBack {
			_ = os.RemoveAll(filepath.Dir(packagePath))
		}
	}
	return outcome, upgradeErr
}

func (c *Client) downloadUpgradePackage(ctx context.Context, intent protocol.UpgradeIntent) (string, error) {
	if intent.Source != protocol.UpgradeSourceOfficialXray || intent.PackageSize == 0 || intent.PackageSize > protocol.MaxUpgradePackageBytes {
		return "", errors.New("升级来源或包大小不受支持")
	}
	available, err := availableDiskBytes(c.core.dir)
	if err != nil || available < intent.PackageSize+coreUpgradeHeadroom {
		return "", fmt.Errorf("升级前可用磁盘空间不足：%d", available)
	}
	url, err := officialXrayReleaseURL(intent.TargetVersion, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("受控 Core 来源返回 HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && uint64(response.ContentLength) != intent.PackageSize {
		return "", errors.New("下载包大小与 Intent 不匹配")
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, int64(intent.PackageSize)+1))
	if err != nil {
		return "", err
	}
	if uint64(len(contents)) != intent.PackageSize || !bytes.Equal(packageDigest(contents), mustDigest(intent.SHA256)) {
		return "", errors.New("下载包校验失败")
	}
	return c.stageUpgradePackage(intent.ID, contents, intent.SHA256)
}

func (c *Client) UpgradePackage(ctx context.Context, intentID, packagePath, targetVersion, packageSHA256 string) (CoreUpgradeOutcome, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.core == nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorCoreDisabled, Err: errors.New("受管 Core 尚未启用")}
	}
	contents, err := os.ReadFile(packagePath)
	if err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorDownload, Err: err}
	}
	if len(contents) > protocol.MaxUpgradePackageBytes || !bytes.Equal(packageDigest(contents), mustDigest(packageSHA256)) {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorChecksum, Err: errors.New("本地升级包校验失败")}
	}
	candidate, err := c.stageUpgradePackage(intentID, contents, packageSHA256)
	if err != nil {
		return CoreUpgradeOutcome{}, &CoreUpgradeError{Code: protocol.UpgradeErrorChecksum, Err: err}
	}
	outcome, upgradeErr := c.core.UpgradeBinary(ctx, intentID, targetVersion, candidate, candidateDigest(candidate))
	if upgradeErr == nil {
		_ = os.RemoveAll(filepath.Dir(candidate))
	} else {
		var typed *CoreUpgradeError
		if errors.As(upgradeErr, &typed) && typed.RolledBack {
			_ = os.RemoveAll(filepath.Dir(candidate))
		}
	}
	return outcome, upgradeErr
}

func (c *Client) stageUpgradePackage(intentID string, contents []byte, expectedPackageSHA string) (string, error) {
	if !security.IsUUIDv4(intentID) || !security.IsValidDigest(expectedPackageSHA) {
		return "", errors.New("升级 Intent 或包校验无效")
	}
	stageDir := filepath.Join(c.core.dir, coreUpgradeDirName, intentID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(stageDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("升级暂存目录不安全")
	}
	packagePath := filepath.Join(stageDir, "package.bin")
	if err := writeUpgradeFile(packagePath, contents, expectedPackageSHA, 0o600); err != nil {
		return "", err
	}
	candidate, err := extractCoreBinary(contents)
	if err != nil {
		// The local CLI may be given an already extracted executable. Remote
		// official releases are ZIPs and therefore still require bounded
		// extraction below.
		candidate = contents
	}
	if len(candidate) == 0 || len(candidate) > protocol.MaxUpgradePackageBytes {
		return "", errors.New("Core 候选文件大小无效")
	}
	candidatePath := filepath.Join(stageDir, "xray")
	if info, err := os.Lstat(candidatePath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("已有 Core 候选路径不安全")
		}
		stored, readErr := os.ReadFile(candidatePath)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(stored, candidate) {
			return "", errors.New("已有 Core 候选文件校验不匹配")
		}
		return candidatePath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := storage.WritePrivateAtomic(candidatePath, candidate); err != nil {
		return "", err
	}
	if err := os.Chmod(candidatePath, 0o700); err != nil {
		return "", err
	}
	return candidatePath, nil
}

func writeUpgradeFile(path string, contents []byte, expectedDigest string, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("升级包暂存路径不安全")
		}
		stored, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Equal(packageDigest(stored), mustDigest(expectedDigest)) {
			return nil
		}
		return errors.New("已有升级包校验不匹配")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func extractCoreBinary(contents []byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > 32 {
		return nil, errors.New("Core 升级包不是受支持的 ZIP")
	}
	var result []byte
	var expanded uint64
	allowed := map[string]bool{"xray": true, "geoip.dat": true, "geosite.dat": true, "LICENSE": true, "README.md": true}
	for _, item := range reader.File {
		if !allowed[item.Name] || item.Name == "" || strings.ContainsAny(item.Name, `/\\`) || item.Mode()&0o170000 != 0 {
			return nil, errors.New("Core 升级包包含未受控条目")
		}
		if item.UncompressedSize64 > protocol.MaxUpgradePackageBytes || expanded > 128<<20-item.UncompressedSize64 {
			return nil, errors.New("Core 升级包解压大小越界")
		}
		expanded += item.UncompressedSize64
		file, openErr := item.Open()
		if openErr != nil {
			return nil, openErr
		}
		if item.Name == "xray" {
			contents, readErr := io.ReadAll(io.LimitReader(file, protocol.MaxUpgradePackageBytes+1))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || uint64(len(contents)) != item.UncompressedSize64 || len(contents) == 0 {
				return nil, errors.New("Core 升级包条目无法读取")
			}
			result = contents
			continue
		}
		readCount, readErr := io.Copy(io.Discard, io.LimitReader(file, int64(item.UncompressedSize64)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || uint64(readCount) != item.UncompressedSize64 {
			return nil, errors.New("Core 升级包条目无法读取")
		}
	}
	if len(result) == 0 {
		return nil, errors.New("Core 升级包缺少 xray 可执行文件")
	}
	return result, nil
}

func officialXrayReleaseURL(version, architecture string) (string, error) {
	if err := validateTargetCoreVersion(version); err != nil {
		return "", err
	}
	asset := map[string]string{"amd64": "Xray-linux-64.zip", "arm64": "Xray-linux-arm64-v8a.zip"}[architecture]
	if asset == "" {
		return "", fmt.Errorf("当前架构不在受控 Core 清单：%s", architecture)
	}
	return officialXrayReleaseBaseURL + version + "/" + asset, nil
}

func packageDigest(contents []byte) []byte {
	digest := sha256.Sum256(contents)
	return digest[:]
}

func candidateDigest(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func mustDigest(value string) []byte {
	decoded, _ := hex.DecodeString(strings.TrimSpace(value))
	return decoded
}

func upgradeErrorDetails(err error) (string, string) {
	var typed *CoreUpgradeError
	if errors.As(err, &typed) {
		return typed.Code, truncateUpgradeDetail(typed.Error())
	}
	return protocol.UpgradeErrorDownload, truncateUpgradeDetail(err.Error())
}

func truncateUpgradeDetail(value string) string {
	if len([]byte(value)) > protocol.MaxUpgradeDetailBytes {
		return string([]byte(value)[:protocol.MaxUpgradeDetailBytes])
	}
	return value
}
