package storage

import (
	"errors"
	"os"
	"syscall"
)

// ChownLike preserves the owner of an existing private path or its parent
// when an administrator runs maintenance on behalf of the service user.
func ChownLike(target, source string) error {
	from, err := os.Lstat(source)
	if err != nil {
		return err
	}
	to, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if from.Mode()&os.ModeSymlink != 0 || to.Mode()&os.ModeSymlink != 0 {
		return errors.New("ownership path must not be a symlink")
	}
	want, ok := from.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("source ownership unavailable")
	}
	actual, ok := to.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("target ownership unavailable")
	}
	if actual.Uid == want.Uid && actual.Gid == want.Gid {
		return nil
	}
	return os.Chown(target, int(want.Uid), int(want.Gid))
}

func RequireOwner(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return errors.New("请以 Agent 数据目录所属用户运行 Core 操作")
	}
	return nil
}
