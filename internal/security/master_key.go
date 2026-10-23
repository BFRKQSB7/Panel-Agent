package security

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	MasterKeyFileName = "master.key"
	MasterKeySize     = 32
)

var ErrDatabaseExistsWithoutKey = errors.New("数据库已存在，但 master.key 缺失")

func LoadOrCreateMasterKey(dataDir, databasePath string) ([]byte, error) {
	path := filepath.Join(dataDir, MasterKeyFileName)
	key, err := readMasterKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	for _, artifact := range []string{databasePath, databasePath + "-wal", databasePath + "-shm", databasePath + "-journal"} {
		if _, statErr := os.Lstat(artifact); statErr == nil {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseExistsWithoutKey, filepath.Base(artifact))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect database artifact: %w", statErr)
		}
	}

	key = make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readMasterKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create master key: %w", err)
	}
	writeErr := writeKey(file, key)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if writeErr != nil {
			return nil, fmt.Errorf("persist master key: %w", writeErr)
		}
		return nil, fmt.Errorf("close master key: %w", closeErr)
	}
	return key, nil
}

func readMasterKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("master key is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("master key permissions must be 0600")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("master key has invalid length")
	}
	return key, nil
}

func writeKey(file *os.File, key []byte) error {
	if _, err := file.Write(key); err != nil {
		return err
	}
	return file.Sync()
}
