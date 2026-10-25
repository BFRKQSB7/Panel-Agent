package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"panel-agent.local/panel-agent/internal/backup"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const panelSelfUpdateJournalName = ".panel-self-update.json"

type panelSelfUpdateJournal struct {
	Version      int    `json:"version"`
	Executable   string `json:"executable"`
	Previous     string `json:"previous"`
	Staged       string `json:"staged"`
	DataStage    string `json:"data_stage"`
	OriginalSHA  string `json:"original_sha256"`
	CandidateSHA string `json:"candidate_sha256"`
	Committed    bool   `json:"committed"`
}

func panelFileDigest(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > protocol.MaxUpgradePackageBytes {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, protocol.MaxUpgradePackageBytes+1)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writePanelSelfUpdateJournal(dir string, journal panelSelfUpdateJournal) error {
	contents, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return storage.WritePrivateAtomic(filepath.Join(dir, panelSelfUpdateJournalName), contents)
}

// Caller holds the Panel process lock. The commit decision covers both the
// executable and D16's DB/key/extension generation; a failed cleanup is retried.
func recoverPanelSelfUpdate(dir, executable string) (bool, error) {
	path := filepath.Join(dir, panelSelfUpdateJournalName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return false, errors.New("Panel 更新 journal 不安全")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var j panelSelfUpdateJournal
	executable, err = filepath.Abs(executable)
	if err != nil {
		return false, err
	}
	if json.Unmarshal(contents, &j) != nil || j.Version != 1 || j.Executable != executable || j.Previous != executable+".previous" || !strings.HasPrefix(j.Staged, executable+".update-") || filepath.Dir(j.Staged) != filepath.Dir(executable) || filepath.Dir(j.DataStage) != dir || !strings.HasPrefix(filepath.Base(j.DataStage), ".panel-restore-stage-") || !security.IsValidDigest(j.OriginalSHA) || !security.IsValidDigest(j.CandidateSHA) {
		return false, errors.New("Panel 更新 journal 身份或路径无效")
	}
	previousDigest := panelFileDigest(j.Previous)
	if j.Committed {
		if !strings.EqualFold(panelFileDigest(executable), j.CandidateSHA) {
			return false, errors.New("已提交 Panel 程序校验失败")
		}
		if err := backup.Commit(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("完成 Panel 数据提交：%w", err)
		}
		if _, err := os.Lstat(j.Previous); err == nil {
			if previousDigest != j.OriginalSHA {
				return false, errors.New("Panel 旧程序校验失败")
			}
			// A Windows updater may still hold its renamed image open.
			if err := os.Remove(j.Previous); err != nil {
				return false, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	} else {
		if previousDigest != j.OriginalSHA && panelFileDigest(executable) != j.OriginalSHA {
			return false, errors.New("Panel 更新丢失匹配的原程序，保留现场")
		}
		if err := backup.RecoverPending(dir); err != nil {
			return false, err
		}
		if previousDigest == j.OriginalSHA {
			if err := rollbackPanelExecutable(executable, j.Previous); err != nil {
				return false, err
			}
		}
	}
	if _, err := os.Lstat(j.Staged); err == nil {
		if !strings.EqualFold(panelFileDigest(j.Staged), j.CandidateSHA) {
			return false, errors.New("Panel 暂存程序校验失败")
		}
		if err := os.Remove(j.Staged); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if info, err := os.Lstat(j.DataStage); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("Panel 数据暂存目录不安全")
		}
		if err := os.RemoveAll(j.DataStage); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return !j.Committed, nil
}
