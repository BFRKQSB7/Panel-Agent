//go:build linux

// guarded-run stops a test process group when its controller stops renewing lease.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type paths []string

func (p *paths) String() string     { return strings.Join(*p, ",") }
func (p *paths) Set(s string) error { *p = append(*p, s); return nil }

func main() { os.Exit(run()) }
func run() int {
	dir := flag.String("dir", "", "new result directory inside the project")
	ttl := flag.Int("lease-seconds", 180, "external controller lease")
	maximum := flag.Int("max-seconds", 9000, "absolute test duration")
	var mounts paths
	flag.Var(&mounts, "unmount", "owned project tmpfs mount to release on exit")
	flag.Parse()
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	inside := func(p string) (string, error) {
		p, e := filepath.Abs(p)
		if e != nil {
			return "", e
		}
		parent, e := filepath.EvalSymlinks(filepath.Dir(p))
		if e != nil {
			return "", e
		}
		p = filepath.Join(parent, filepath.Base(p))
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return "", fmt.Errorf("outside project: %s", p)
		}
		return p, nil
	}
	if *ttl < 1 || *ttl > 300 || *maximum < 1 || *maximum > 10800 || len(flag.Args()) == 0 {
		return 2
	}
	resultDir, err := inside(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for i, p := range mounts {
		mounts[i], err = inside(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		// Accept only ordinary project directories; never follow a final symlink.
		info, e := os.Lstat(mounts[i])
		if e != nil || !info.IsDir() {
			return 2
		}
	}
	if err = os.Mkdir(resultDir, 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	lease := filepath.Join(resultDir, "lease")
	if err = os.WriteFile(lease, []byte("Renew only from the external controller.\n"), 0600); err != nil {
		return 2
	}
	started := time.Now()
	status := func(reason string, code int) {
		b, _ := json.MarshalIndent(map[string]any{"reason": reason, "exit_code": code, "started_at": started.UTC(), "updated_at": time.Now().UTC(), "elapsed_seconds": time.Since(started).Seconds()}, "", "  ")
		p := filepath.Join(resultDir, "status.json")
		if e := os.WriteFile(p+".tmp", b, 0600); e == nil {
			_ = os.Rename(p+".tmp", p)
		}
	}
	output, err := os.OpenFile(filepath.Join(resultDir, "runner.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	defer output.Close()
	args := flag.Args()
	// Core processes may create another session. Adopt them if their test parent
	// exits, so they remain owned by this supervisor rather than escaping to PID 1.
	if err = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(output, err)
		return 1
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Stdout, cmd.Stderr = output, output
	if err = cmd.Start(); err != nil {
		fmt.Fprintln(output, err)
		status("start_failed", 1)
		return 1
	}
	status("running", 0)
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	reason, code, done := "completed", 0, false
wait:
	for {
		select {
		case e := <-finished:
			done = true
			if e != nil {
				reason = "test_failed"
				code = 1
			}
			break wait
		case <-signals:
			reason = "controller_stopped"
			code = 130
			break wait
		case <-tick.C:
			if time.Since(started) >= time.Duration(*maximum)*time.Second {
				reason = "hard_timeout"
				code = 124
				break wait
			}
			info, e := os.Stat(lease)
			if e != nil || time.Since(info.ModTime()) >= time.Duration(*ttl)*time.Second {
				reason = "lease_expired"
				code = 124
				break wait
			}
			if _, e = os.Stat(filepath.Join(resultDir, "stop")); e == nil {
				reason = "controller_stopped"
				code = 130
				break wait
			}
		}
	}
	// The child stays the sole leader of this private group. Stop descendants even
	// when its leader has already exited, so failed scripts cannot orphan a VM.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if !done {
		select {
		case <-finished:
			done = true
		case <-time.After(10 * time.Second):
		}
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if !done {
		select {
		case <-finished:
			done = true
		case <-time.After(2 * time.Second):
		}
	}
	if !done {
		code, reason = 1, reason+";child_wait_failed"
	} else if err = stopAdoptedChildren(); err != nil {
		fmt.Fprintln(output, err)
		code, reason = 1, reason+";descendant_cleanup_failed"
	}
	for _, p := range mounts {
		if exec.Command("mountpoint", "-q", p).Run() != nil {
			continue
		}
		// Only release a tmpfs mounted exactly on this owned directory.
		kind, e := exec.Command("findmnt", "-n", "-o", "FSTYPE", "--mountpoint", p).Output()
		if e != nil || strings.TrimSpace(string(kind)) != "tmpfs" {
			code = 1
			reason += ";mount_not_tmpfs"
			continue
		}
		if e = exec.Command("umount", p).Run(); e != nil {
			code = 1
			reason += ";unmount_failed"
		}
	}
	status(reason, code)
	fmt.Printf("GUARDED_RUN_STOPPED reason=%s exit=%d\n", reason, code)
	return code
}

func stopAdoptedChildren() error {
	for attempt := 0; attempt < 30; attempt++ {
		// The original exec.Cmd has already been waited for. Only adopted test
		// children remain, so no concurrent exec waiter can lose its child here.
		for {
			pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		files, err := filepath.Glob("/proc/self/task/*/children")
		if err != nil {
			return err
		}
		children := map[int]bool{}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if os.IsNotExist(err) {
				continue // A Go runtime thread may exit during enumeration.
			}
			if err != nil {
				return err
			}
			for _, field := range strings.Fields(string(b)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					return err
				}
				children[pid] = true
			}
		}
		if len(children) == 0 {
			return nil
		}
		for pid := range children {
			fd, err := unix.PidfdOpen(pid, 0)
			if err == unix.ESRCH {
				continue
			}
			if err != nil {
				return err
			}
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
			owned := false
			if err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(line, "PPid:") {
						parent, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
						owned = parent == os.Getpid()
					}
				}
			}
			if owned {
				signal := unix.SIGTERM
				if attempt >= 5 {
					signal = unix.SIGKILL
				}
				err = unix.PidfdSendSignal(fd, signal, nil, 0)
			}
			unix.Close(fd)
			if err != nil && err != unix.ESRCH && !os.IsNotExist(err) {
				return err
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("test descendants remain after cleanup")
}
