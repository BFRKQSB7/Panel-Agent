package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

type CleanupResult struct {
	Removed []string
	Rotated []string
}

// Cleanup removes only known regenerable files. It deliberately does not scan
// or delete the database, keys, Core journal, Current/LKG configs, or ledger
// records, including when the disk is low.
func Cleanup(dataDir, logRoot string, now time.Time) (CleanupResult, error) {
	dataDir, err := absoluteClean(dataDir)
	if err != nil {
		return CleanupResult{}, err
	}
	logRoot, err = absoluteClean(logRoot)
	if err != nil {
		return CleanupResult{}, err
	}
	if info, err := os.Lstat(dataDir); errors.Is(err, os.ErrNotExist) {
		return CleanupResult{}, nil
	} else if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return CleanupResult{}, fmt.Errorf("%w: %s", ErrUnsafePath, dataDir)
	} else if err := ensurePathDirectory(dataDir, 0o700, true); err != nil {
		return CleanupResult{}, err
	}
	lock, err := storage.AcquireDataLock(dataDir, ".cleanup.lock")
	if err != nil {
		return CleanupResult{}, err
	}
	defer lock.Close()
	if info, err := os.Lstat(logRoot); errors.Is(err, os.ErrNotExist) {
		logRoot = ""
	} else if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return CleanupResult{}, fmt.Errorf("%w: %s", ErrUnsafePath, logRoot)
	} else if err := ensurePathDirectory(logRoot, 0o750, false); err != nil {
		return CleanupResult{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := CleanupResult{}
	for _, name := range []string{"tmp", "downloads"} {
		removed, err := cleanRegenerableDirectory(filepath.Join(dataDir, name), func(entry os.DirEntry, info os.FileInfo) bool {
			return info.Mode().IsRegular() && (strings.HasSuffix(entry.Name(), ".tmp") || strings.HasSuffix(entry.Name(), ".part") || strings.HasSuffix(entry.Name(), ".download"))
		})
		if err != nil {
			return result, err
		}
		result.Removed = append(result.Removed, removed...)
	}
	removed, err := cleanRegenerableDirectory(filepath.Join(dataDir, "debug"), func(entry os.DirEntry, info os.FileInfo) bool {
		return info.Mode().IsRegular() && strings.HasPrefix(entry.Name(), "debug-") && now.Sub(info.ModTime()) >= 24*time.Hour
	})
	if err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, removed...)
	logPaths := []string{filepath.Join(dataDir, "core", "core.log")}
	if logRoot != "" {
		logPaths = append(logPaths, filepath.Join(logRoot, "control.log"), filepath.Join(logRoot, "agent.log"))
	}
	for _, path := range logPaths {
		rotated, err := rotateLog(path, MaxCleanupLogBytes, 2)
		if err != nil {
			return result, err
		}
		if rotated {
			result.Rotated = append(result.Rotated, path)
		}
	}
	return result, nil
}

func cleanRegenerableDirectory(path string, keep func(os.DirEntry, os.FileInfo) bool) ([]string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrUnsafePath, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		entryInfo, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return removed, fmt.Errorf("%w: %s", ErrUnsafePath, entryPath)
		}
		if keep(entry, entryInfo) {
			if err := os.Remove(entryPath); err != nil {
				return removed, err
			}
			removed = append(removed, entryPath)
		}
	}
	return removed, nil
}

func rotateLog(path string, maxBytes int64, keep int) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %s", ErrUnsafePath, path)
	}
	if info.Size() <= maxBytes {
		return false, nil
	}
	if keep < 1 {
		keep = 1
	}
	for index := keep - 1; index >= 1; index-- {
		oldPath := path + fmt.Sprintf(".%d", index)
		newPath := path + fmt.Sprintf(".%d", index+1)
		if _, err := os.Lstat(oldPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		if err := removeRegularOnly(newPath); err != nil {
			return false, err
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return false, err
		}
	}
	rotated := path + ".1"
	if err := removeRegularOnly(rotated); err != nil {
		return false, err
	}
	source, err := os.Open(path)
	if err != nil {
		return false, err
	}
	destination, err := os.OpenFile(rotated, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = source.Close()
		return false, err
	}
	_, copyErr := io.Copy(destination, source)
	syncErr := destination.Sync()
	closeDestinationErr := destination.Close()
	closeSourceErr := source.Close()
	if copyErr != nil {
		return false, copyErr
	}
	if syncErr != nil {
		return false, syncErr
	}
	if closeDestinationErr != nil || closeSourceErr != nil {
		return false, errors.Join(closeDestinationErr, closeSourceErr)
	}
	current, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false, err
	}
	truncateErr := current.Truncate(0)
	if truncateErr == nil {
		truncateErr = current.Sync()
	}
	closeErr := current.Close()
	return true, errors.Join(truncateErr, closeErr)
}

func removeRegularOnly(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrUnsafePath, path)
	}
	return os.Remove(path)
}
