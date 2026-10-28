package hysteria

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

type processIdentity struct {
	pid         int
	start, boot string
}

func identifyProcess(pid int, binary, config string) (processIdentity, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processIdentity{}, err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return processIdentity{}, errors.New("hysteria: invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return processIdentity{}, errors.New("hysteria: incomplete process stat")
	}
	if fields[0] == "Z" {
		return processIdentity{}, os.ErrNotExist
	}
	command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return processIdentity{}, err
	}
	expected := []byte(binary + "\x00server\x00--config\x00" + config + "\x00")
	if !bytes.Equal(command, expected) {
		return processIdentity{}, errors.New("hysteria: process command does not belong to this instance")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processIdentity{}, err
	}
	if strings.TrimSuffix(exe, " (deleted)") != binary {
		return processIdentity{}, errors.New("hysteria: executable identity mismatch")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(bytes.TrimSpace(boot)) == 0 {
		return processIdentity{}, errors.New("hysteria: boot identity unavailable")
	}
	return processIdentity{pid: pid, start: fields[19], boot: strings.TrimSpace(string(boot))}, nil
}

// StopOrphans is called under the machine's exclusive Agent data lock. It only
// targets an exact managed binary/command/config tuple, never a process name or
// a port owner. A successful return precedes finalization of its ledger source.
func StopOrphans(ctx context.Context, binary, instanceDir string) error {
	if ctx == nil || !filepath.IsAbs(binary) || !filepath.IsAbs(instanceDir) {
		return errors.New("hysteria: invalid recovery context")
	}
	if err := storage.RequireOwner(instanceDir); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(instanceDir)
	if err != nil || resolved != instanceDir {
		return errors.New("hysteria: unsafe recovery directory")
	}
	config := filepath.Join(instanceDir, "server.json")
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !bytes.Equal(command, []byte(binary+"\x00server\x00--config\x00"+config+"\x00")) {
			continue
		}
		identity, err := identifyProcess(pid, binary, config)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		handle, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		actual, err := identifyProcess(pid, binary, config)
		if errors.Is(err, os.ErrNotExist) {
			handle.Release()
			continue
		}
		if err != nil || actual != identity {
			handle.Release()
			return errors.New("hysteria: orphan identity changed during recovery")
		}
		if err := handle.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			handle.Release()
			return err
		}
		for {
			// cmdline can become empty during exit before procfs removes stat.
			// After signalling the verified handle, wait on start ticks/state;
			// a reused PID proves the original exited and must never be killed.
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				handle.Release()
				return errors.New("hysteria: cannot confirm orphan exit")
			}
			end := bytes.LastIndexByte(stat, ')')
			if end >= 0 {
				fields := strings.Fields(string(stat[end+1:]))
				if len(fields) >= 20 && (fields[0] == "Z" || fields[0] == "X" || fields[19] != identity.start) {
					break
				}
			}
			select {
			case <-ctx.Done():
				handle.Release()
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		handle.Release()
	}
	return nil
}
