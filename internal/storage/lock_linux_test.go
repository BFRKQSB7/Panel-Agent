//go:build linux

package storage

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestAcquireDataLockPreservesDirectoryOwnerForPrivilegedUpdater(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to model an administrator updating a service-owned directory")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody account unavailable: %v", err)
	}
	uid, err := strconv.Atoi(nobody.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(nobody.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireDataLock(dir, "agent.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	info, err := os.Stat(filepath.Join(dir, "agent.lock"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("lock owner = %v, want %d:%d", info.Sys(), uid, gid)
	}
}
