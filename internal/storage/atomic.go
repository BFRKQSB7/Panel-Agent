package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"panel-agent.local/panel-agent/internal/security"
)

// WritePrivateAtomic persists one private file, including the parent-directory
// rename on Linux. The caller owns serialization of the surrounding state.
func WritePrivateAtomic(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("private file must be regular")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	token, err := security.NewToken(16)
	if err != nil {
		return err
	}
	temporary := path + "." + token + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer directory.Close()
		if err := directory.Sync(); err != nil {
			return fmt.Errorf("sync private file directory: %w", err)
		}
	}
	return nil
}
