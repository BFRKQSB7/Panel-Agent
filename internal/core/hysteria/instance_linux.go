package hysteria

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

var ErrRestartRequired = errors.New("hysteria: permission change stopped instance; restart required")

// Instance owns one child and its local authentication server. The Agent must
// durably establish its ledger source before calling StartInstance, and finalize
// that source after Stop. This type does not ACK desired-state revisions.
type Instance struct {
	updateMu      sync.Mutex
	process       *os.Process
	done          chan struct{}
	stopOnce      sync.Once
	stopping      chan struct{}
	auth          *Authenticator
	server        *http.Server
	api           *API
	expiryChanged chan time.Time
}

// StartInstance accepts a previously verified, project-owned official binary.
// The caller allocates the directory and ports; no public management listener,
// binary download, service installation or firewall modification happens here.
func StartInstance(ctx context.Context, binary string, options ServerOptions, accounts []Account) (*Instance, error) {
	if ctx == nil {
		return nil, errors.New("hysteria: missing lifecycle context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(binary) {
		return nil, errors.New("hysteria: binary path must be absolute")
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("hysteria: unsafe executable")
	}
	files, err := RenderServer(options)
	if err != nil {
		return nil, err
	}
	if err := storage.RequireOwner(options.InstanceDir); err != nil {
		return nil, err
	}
	resolvedDir, err := filepath.EvalSymlinks(options.InstanceDir)
	if err != nil || resolvedDir != options.InstanceDir {
		return nil, errors.New("hysteria: instance directory must not traverse symlinks")
	}
	dirInfo, err := os.Lstat(options.InstanceDir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("hysteria: instance directory must be private")
	}
	lockPath := filepath.Join(options.InstanceDir, "instance.lock")
	if existing, err := os.Lstat(lockPath); err == nil {
		if !existing.Mode().IsRegular() || existing.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("hysteria: unsafe instance lock")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock, err := storage.AcquireDataLock(options.InstanceDir, "instance.lock")
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			_ = lock.Close()
		}
	}()
	// Do not overwrite configuration of a leftover child still holding its UDP
	// socket after an Agent crash. The supervisor must reconcile it first.
	probe, err := net.ListenPacket("udp", net.JoinHostPort(options.ListenHost, fmt.Sprint(options.ListenPort)))
	if err != nil {
		return nil, errors.New("hysteria: UDP listener unavailable")
	}
	if err := probe.Close(); err != nil {
		return nil, err
	}
	auth, err := NewAuthenticator(options.AuthRouteToken)
	if err != nil {
		return nil, err
	}
	if _, err = auth.Replace(accounts); err != nil {
		return nil, err
	}
	api, err := NewAPI(options.StatsAddress, options.StatsSecret)
	if err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{"server.json": files.Config, "server.crt": files.Certificate, "server.key": files.PrivateKey} {
		p := filepath.Join(options.InstanceDir, name)
		if existing, statErr := os.Lstat(p); statErr == nil {
			if !existing.Mode().IsRegular() || existing.Mode().Perm()&0o077 != 0 {
				api.Close()
				return nil, errors.New("hysteria: unsafe instance file")
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			api.Close()
			return nil, statErr
		}
		if err := storage.WritePrivateAtomic(p, data); err != nil {
			api.Close()
			return nil, err
		}
	}
	listener, err := net.Listen("tcp", options.AuthAddress)
	if err != nil {
		api.Close()
		return nil, errors.New("hysteria: auth listener unavailable")
	}
	server := &http.Server{Handler: auth, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10}
	i := &Instance{done: make(chan struct{}), stopping: make(chan struct{}), auth: auth, server: server, api: api, expiryChanged: make(chan time.Time, 1)}
	// Discard Core output here rather than risking credentials or unbounded logs.
	command := exec.Command(binary, "server", "--config", filepath.Join(options.InstanceDir, "server.json"))
	command.Dir = options.InstanceDir
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		listener.Close()
		api.Close()
		return nil, errors.New("hysteria: cannot start official executable")
	}
	i.process = command.Process
	started = true
	go func() {
		_ = command.Wait()
		_ = server.Close()
		api.Close()
		_ = lock.Close()
		close(i.done)
	}()
	go func() {
		if err := server.Serve(listener); err != nil {
			// Even an unexpected authentication-server failure must fence old sessions.
			i.stop()
		}
	}()
	go i.watch(ctx, earliestExpiry(accounts))
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if _, err := i.Snapshot(startup); err == nil && ownsUDP(command.Process.Pid, options.ListenHost, options.ListenPort) == nil {
			return i, nil
		}
		select {
		case <-i.done:
			return nil, errors.New("hysteria: process exited before health confirmation")
		case <-startup.Done():
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
			stopErr := i.Stop(cleanup)
			cancelCleanup()
			return nil, errors.Join(errors.New("hysteria: startup health failed"), stopErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (i *Instance) stop() {
	i.stopOnce.Do(func() { close(i.stopping); _ = i.server.Close(); _ = i.process.Kill() })
}

// Stop fences the owned child handle and waits for exit, including in-flight
// auth. A caller timeout is an error, never proof that the instance has stopped.
func (i *Instance) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("hysteria: missing stop context")
	}
	i.stop()
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i *Instance) Snapshot(ctx context.Context) (map[string]Counter, error) {
	select {
	case <-i.stopping:
		return nil, errors.New("hysteria: instance stopping")
	case <-i.done:
		return nil, errors.New("hysteria: instance exited")
	default:
	}
	readings, err := i.api.Traffic(ctx)
	if err != nil {
		return nil, err
	}
	select {
	case <-i.done:
		return nil, errors.New("hysteria: instance exited during sampling")
	default:
		return readings, nil
	}
}

// ReplaceAccounts stops the whole instance on revocation to fence auth/kick
// races. The supervisor must sample before changing the snapshot, then restart
// under a new ledger source. Additions alone need no process restart.
func (i *Instance) ReplaceAccounts(ctx context.Context, accounts []Account) error {
	if ctx == nil {
		return errors.New("hysteria: missing update context")
	}
	i.updateMu.Lock()
	defer i.updateMu.Unlock()
	select {
	case <-i.stopping:
		return errors.New("hysteria: instance stopping")
	case <-i.done:
		return errors.New("hysteria: instance exited")
	default:
	}
	revoked, err := i.auth.Replace(accounts)
	if err != nil {
		return err
	}
	if len(revoked) > 0 {
		if err := i.Stop(ctx); err != nil {
			return err
		}
		return ErrRestartRequired
	}
	select {
	case i.expiryChanged <- earliestExpiry(accounts):
	case <-i.done:
		return errors.New("hysteria: instance exited")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func earliestExpiry(accounts []Account) time.Time {
	var next time.Time
	for _, a := range accounts {
		if !a.ExpiresAt.IsZero() && (next.IsZero() || a.ExpiresAt.Before(next)) {
			next = a.ExpiresAt
		}
	}
	return next
}

func (i *Instance) watch(ctx context.Context, expiry time.Time) {
	for {
		var timer *time.Timer
		var tick <-chan time.Time
		if !expiry.IsZero() {
			timer = time.NewTimer(time.Until(expiry))
			tick = timer.C
		}
		select {
		case expiry = <-i.expiryChanged:
		case <-tick:
			i.stop()
		case <-ctx.Done():
			i.stop()
		case <-i.done:
			if timer != nil {
				timer.Stop()
			}
			return
		}
		if timer != nil {
			timer.Stop()
		}
		select {
		case <-i.stopping:
			return
		default:
		}
	}
}

// ownsUDP requires the actual child to own the expected bound socket; a UDP
// dial or a TCP API response alone cannot establish readiness of a QUIC listener.
func ownsUDP(pid int, host string, port int) error {
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	ip := net.ParseIP(host)
	var expected []byte
	table := "udp6"
	if v4 := ip.To4(); v4 != nil {
		expected = v4
		table = "udp"
	} else {
		expected = ip.To16()
	}
	// Linux procfs represents each 32-bit address word in host byte order.
	var encoded strings.Builder
	for offset := 0; offset < len(expected); offset += 4 {
		for j := 3; j >= 0; j-- {
			fmt.Fprintf(&encoded, "%02X", expected[offset+j])
		}
	}
	address := fmt.Sprintf("%s:%04X", encoded.String(), port)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
	if err != nil {
		return err
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) > 9 && fields[1] == address && inodes[fields[9]] {
			return nil
		}
	}
	return errors.New("hysteria: child does not own expected UDP listener")
}
