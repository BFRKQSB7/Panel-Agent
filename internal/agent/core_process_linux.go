package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const managedCoreSupported = true

var errCoreNotRunning = errors.New("Core process is not running")

type coreProcess struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"start_ticks"`
	ConfigPath string `json:"config_path"`
	BootID     string `json:"boot_id,omitempty"`
}

func inspectCoreProcess(pid int, binary string) (coreProcess, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return coreProcess{}, err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return coreProcess{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return coreProcess{}, errCoreNotRunning
	}
	command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return coreProcess{}, err
	}
	args := bytes.Split(bytes.TrimSuffix(command, []byte{0}), []byte{0})
	if len(args) != 4 || string(args[0]) != binary || string(args[1]) != "run" || string(args[2]) != "-config" {
		return coreProcess{}, errors.New("process does not match the managed Core")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(bytes.TrimSpace(boot)) == 0 {
		return coreProcess{}, errors.New("cannot prove Core boot identity")
	}
	return coreProcess{PID: pid, StartTicks: fields[19], ConfigPath: string(args[3]), BootID: strings.TrimSpace(string(boot))}, nil
}

func findCoreProcesses(binary string, configPaths map[string]bool) ([]coreProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []coreProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		process, err := inspectCoreProcess(pid, binary)
		if err == nil && configPaths[process.ConfigPath] {
			processes = append(processes, process)
		}
	}
	return processes, nil
}

func stopCoreProcess(ctx context.Context, process coreProcess, binary string) error {
	// FindProcess obtains a pidfd on the supported Linux test host. Verify the
	// process after obtaining the handle so PID reuse cannot select another Core.
	handle, err := os.FindProcess(process.PID)
	if err != nil {
		return err
	}
	defer handle.Release()
	actual, err := inspectCoreProcess(process.PID, binary)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errCoreNotRunning) {
		return nil
	}
	if err != nil || actual != process {
		return errors.New("refusing to stop an unrelated process")
	}
	if err := handle.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	forced := false
	for {
		if _, err := inspectCoreProcess(process.PID, binary); err != nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil && !forced {
			if forced {
				return errors.New("Core did not exit after SIGKILL")
			}
			if err := handle.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
			forced = true
			deadline = time.Now().Add(3 * time.Second)
		}
		select {
		case <-time.After(30 * time.Millisecond):
		}
	}
}

func startCoreCommand(binary, configPath string, logPaths ...string) (coreProcess, error) {
	outputPath := os.DevNull
	if len(logPaths) > 0 && logPaths[0] != "" {
		outputPath = logPaths[0]
		info, err := os.Lstat(outputPath)
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return coreProcess{}, errors.New("Core log must be a regular file")
		}
		if err == nil && info.Mode().Perm()&0o077 != 0 {
			return coreProcess{}, errors.New("Core log permissions must be 0600")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return coreProcess{}, err
		}
	}
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return coreProcess{}, err
	}
	defer output.Close()
	command := exec.Command(binary, "run", "-config", configPath)
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return coreProcess{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	// /proc stat and cmdline are separate reads; immediately after exec they
	// need not form a stable snapshot. Never persist an unverified identity.
	deadline := time.Now().Add(time.Second)
	for {
		process, err := inspectCoreProcess(command.Process.Pid, binary)
		if err == nil {
			return process, nil
		}
		select {
		case exitErr := <-exited:
			return coreProcess{}, fmt.Errorf("Core exited during startup: %v", exitErr)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			return coreProcess{}, fmt.Errorf("Core process identity did not stabilize: %w", err)
		}
	}
}
