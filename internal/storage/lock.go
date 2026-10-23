package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type ProcessLock struct {
	file *os.File
	once sync.Once
	err  error
}

func AcquireProcessLock(dataDir string) (*ProcessLock, error) {
	path := filepath.Join(dataDir, "panel.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire process lock (another Panel process may be running): %w", err)
	}
	return &ProcessLock{file: file}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := unlockFile(l.file); err != nil {
			l.err = err
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
