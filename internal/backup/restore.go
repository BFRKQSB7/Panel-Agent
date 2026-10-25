package backup

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const (
	restoreJournalName = ".panel-restore-journal.json"
	restorePrefix      = ".panel-restore-"
)

type StageResult struct {
	Path     string
	Manifest Manifest
}

type restoreJournal struct {
	Version  int    `json:"version"`
	DataDir  string `json:"data_dir"`
	StageDir string `json:"stage_dir"`
	OldDir   string `json:"old_dir"`
	State    string `json:"state"`
}

// Stage validates a bundle in a private sibling directory. It opens the
// staged database with the real storage code and decrypts all persisted secret
// families before the active data directory can be touched.
func Stage(ctx context.Context, dataDir string, bundle Bundle, expectedInstallation string) (StageResult, error) {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return StageResult{}, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return StageResult{}, fmt.Errorf("restore data directory is unsafe: %s", absolute)
	}
	if expectedInstallation != "" && bundle.Manifest.InstallationID != expectedInstallation {
		return StageResult{}, errors.New("备份 installation_id 与当前 Panel 不匹配")
	}
	if expectedInstallation != "" {
		currentKey, keyErr := os.ReadFile(filepath.Join(absolute, "master.key"))
		if keyErr != nil || len(currentKey) != len(bundle.MasterKey) || subtle.ConstantTimeCompare(currentKey, bundle.MasterKey) != 1 {
			return StageResult{}, errors.New("备份 master.key 与当前 Panel 不匹配")
		}
	}
	stage, err := os.MkdirTemp(absolute, restorePrefix+"stage-")
	if err != nil {
		return StageResult{}, err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = os.RemoveAll(stage)
		return StageResult{}, err
	}
	if err := storage.ChownLike(stage, absolute); err != nil {
		os.RemoveAll(stage)
		return StageResult{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := writePrivateFile(filepath.Join(stage, "panel.db"), bundle.Database); err != nil {
		return StageResult{}, err
	}
	if err := writePrivateFile(filepath.Join(stage, "master.key"), bundle.MasterKey); err != nil {
		return StageResult{}, err
	}
	stagedStore, err := storage.Open(stage)
	if err != nil {
		return StageResult{}, fmt.Errorf("open staged Panel database: %w", err)
	}
	if err := stagedStore.ValidateIntegrity(ctx); err != nil {
		_ = stagedStore.Close()
		return StageResult{}, fmt.Errorf("validate staged Panel state: %w", err)
	}
	installation, err := stagedStore.InstallationID(ctx)
	if err != nil {
		_ = stagedStore.Close()
		return StageResult{}, err
	}
	if installation != bundle.Manifest.InstallationID {
		_ = stagedStore.Close()
		return StageResult{}, errors.New("备份 installation_id 与数据库不一致")
	}
	stagedSnapshot, err := stagedStore.Snapshot(ctx)
	if err != nil {
		_ = stagedStore.Close()
		return StageResult{}, fmt.Errorf("validate staged extension snapshot: %w", err)
	}
	if len(stagedSnapshot.Extensions) != len(bundle.Extensions) {
		_ = stagedStore.Close()
		return StageResult{}, errors.New("备份扩展资源与数据库不一致")
	}
	if stagedSnapshot.SchemaVersion < bundle.Manifest.SchemaVersion {
		_ = stagedStore.Close()
		return StageResult{}, errors.New("备份数据库 schema 版本高于当前 Panel")
	}
	for key, contents := range bundle.Extensions {
		if !bytes.Equal(contents, stagedSnapshot.Extensions[key]) {
			_ = stagedStore.Close()
			return StageResult{}, fmt.Errorf("备份扩展资源与数据库不一致：%s", key)
		}
	}
	if err := stagedStore.Close(); err != nil {
		return StageResult{}, err
	}
	keep = true
	return StageResult{Path: stage, Manifest: bundle.Manifest}, nil
}

// Swap moves only the two private Panel generations. A journal is retained
// until Commit, so a process termination before authority finalization rolls
// back to the complete old generation on the next start.
func Swap(dataDir, stage string) error {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	stage, err = filepath.Abs(stage)
	if err != nil || !safeRestoreChild(absolute, stage, "stage-") {
		return errors.New("restore stage path is unsafe")
	}
	if _, err := os.Lstat(filepath.Join(stage, "panel.db")); err != nil {
		return errors.New("restore stage lacks panel.db")
	}
	if _, err := os.Lstat(filepath.Join(stage, "master.key")); err != nil {
		return errors.New("restore stage lacks master.key")
	}
	journalPath := filepath.Join(absolute, restoreJournalName)
	if _, err := os.Lstat(journalPath); err == nil {
		return errors.New("another restore is pending")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	token, err := security.NewToken(16)
	if err != nil {
		return err
	}
	oldDir := filepath.Join(absolute, restorePrefix+"old-"+token)
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		return err
	}
	if err := storage.ChownLike(oldDir, absolute); err != nil {
		os.Remove(oldDir)
		return err
	}
	j := restoreJournal{Version: 1, DataDir: absolute, StageDir: stage, OldDir: oldDir, State: "started"}
	if err := writeJournal(journalPath, j); err != nil {
		_ = os.RemoveAll(oldDir)
		return err
	}
	if err := moveCurrentFiles(absolute, oldDir); err != nil {
		_ = rollbackJournal(journalPath, j)
		return err
	}
	j.State = "old_moved"
	if err := writeJournal(journalPath, j); err != nil {
		_ = rollbackJournal(journalPath, j)
		return err
	}
	for _, name := range []string{"panel.db", "master.key"} {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(absolute, name)); err != nil {
			_ = rollbackJournal(journalPath, j)
			return err
		}
	}
	j.State = "new_moved"
	if err := writeJournal(journalPath, j); err != nil {
		_ = rollbackJournal(journalPath, j)
		return err
	}
	if err := validateDatabasePair(absolute); err != nil {
		_ = rollbackJournal(journalPath, j)
		return fmt.Errorf("validate swapped Panel generation: %w", err)
	}
	return nil
}

// Commit makes the new generation durable after the caller has finalized the
// authority and reopened the database. Cleanup is recoverable if interrupted.
func Commit(dataDir string) error {
	j, journalPath, err := readJournal(dataDir)
	if err != nil {
		return err
	}
	if j.State == "committed" {
		_ = cleanupCommitted(journalPath, j)
		return nil
	}
	if j.State != "new_moved" {
		return errors.New("restore is not ready to commit")
	}
	j.State = "committed"
	if err := writeJournal(journalPath, j); err != nil {
		return err
	}
	// The generation is already committed at this point. Cleanup is
	// recoverable from the committed journal on the next startup and must not
	// turn a successful restore into an in-process rollback attempt.
	_ = cleanupCommitted(journalPath, j)
	return nil
}

// Rollback restores the old generation after an in-process validation or
// authority-finalization error.
func Rollback(dataDir string) error {
	j, journalPath, err := readJournal(dataDir)
	if err != nil {
		return err
	}
	return rollbackJournal(journalPath, j)
}

// RecoverPending is called before opening the Panel store. Only a journal that
// reached the committed state keeps the new generation; every earlier state
// restores the old pair and removes the incomplete stage.
func RecoverPending(dataDir string) error {
	j, journalPath, err := readJournal(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.State == "committed" {
		return cleanupCommitted(journalPath, j)
	}
	return rollbackJournal(journalPath, j)
}

func moveCurrentFiles(dataDir, oldDir string) error {
	for _, name := range []string{"panel.db", "panel.db-wal", "panel.db-shm", "panel.db-journal", "master.key"} {
		path := filepath.Join(dataDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("restore source is not a regular file: %s", path)
		}
		if err := os.Rename(path, filepath.Join(oldDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func rollbackJournal(journalPath string, j restoreJournal) error {
	var result error
	for _, name := range []string{"panel.db", "panel.db-wal", "panel.db-shm", "panel.db-journal", "master.key"} {
		path := filepath.Join(j.DataDir, name)
		if _, oldErr := os.Lstat(filepath.Join(j.OldDir, name)); errors.Is(oldErr, os.ErrNotExist) {
			continue
		}
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				result = errors.Join(result, fmt.Errorf("cannot remove unsafe restore file: %s", path))
				continue
			}
			result = errors.Join(result, os.Remove(path))
		} else if !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	for _, name := range []string{"panel.db", "panel.db-wal", "panel.db-shm", "panel.db-journal", "master.key"} {
		from := filepath.Join(j.OldDir, name)
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, os.Rename(from, filepath.Join(j.DataDir, name)))
	}
	result = errors.Join(result, removeRestoreDir(j.StageDir, j.DataDir, "stage-"))
	result = errors.Join(result, removeRestoreDir(j.OldDir, j.DataDir, "old-"))
	result = errors.Join(result, os.Remove(journalPath))
	return result
}

func cleanupCommitted(journalPath string, j restoreJournal) error {
	result := removeRestoreDir(j.StageDir, j.DataDir, "stage-")
	result = errors.Join(result, removeRestoreDir(j.OldDir, j.DataDir, "old-"))
	result = errors.Join(result, os.Remove(journalPath))
	if errors.Is(result, os.ErrNotExist) {
		return nil
	}
	return result
}

func readJournal(dataDir string) (restoreJournal, string, error) {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return restoreJournal{}, "", err
	}
	path := filepath.Join(absolute, restoreJournalName)
	contents, err := os.ReadFile(path)
	if err != nil {
		return restoreJournal{}, path, err
	}
	var journal restoreJournal
	if err := json.Unmarshal(contents, &journal); err != nil || journal.Version != 1 || journal.DataDir != absolute || !safeRestoreChild(absolute, journal.StageDir, "stage-") || !safeRestoreChild(absolute, journal.OldDir, "old-") || (journal.State != "started" && journal.State != "old_moved" && journal.State != "new_moved" && journal.State != "committed") {
		return restoreJournal{}, path, errors.New("restore journal is invalid")
	}
	return journal, path, nil
}

func writeJournal(path string, journal restoreJournal) error {
	contents, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := storage.ChownLike(temporary, filepath.Dir(path)); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	return os.Rename(temporary, path)
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
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validateDatabasePair(dataDir string) error {
	key, err := os.ReadFile(filepath.Join(dataDir, "master.key"))
	if err != nil || len(key) != security.MasterKeySize {
		return errors.New("master.key is invalid")
	}
	path := filepath.Join(dataDir, "panel.db")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("panel.db is invalid")
	}
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(uriPath) >= 2 && uriPath[1] == ':' {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", "file:"+uriPath+"?mode=ro&_query_only=1")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		return errors.New("panel.db has no schema migrations")
	}
	return nil
}

func safeRestoreChild(root, child, suffix string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteChild, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteChild)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.Dir(relative) != "." {
		return false
	}
	return strings.HasPrefix(filepath.Base(relative), restorePrefix) && strings.Contains(filepath.Base(relative), suffix)
}

func removeRestoreDir(path, root, suffix string) error {
	if !safeRestoreChild(root, path, suffix) {
		return errors.New("unsafe restore cleanup path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("restore cleanup path is not a directory")
	}
	return os.RemoveAll(path)
}
