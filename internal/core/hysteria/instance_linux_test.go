package hysteria

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnsUDPRequiresActualBoundSocket(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "0.0.0.0", "::1"} {
		t.Run(host, func(t *testing.T) {
			socket, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("address family unavailable: %v", err)
			}
			defer socket.Close()
			port := socket.LocalAddr().(*net.UDPAddr).Port
			if err := ownsUDP(os.Getpid(), host, port); err != nil {
				t.Fatal(err)
			}
			if err := ownsUDP(-1, host, port); err == nil {
				t.Fatal("accepted unrelated process")
			}
			if err := socket.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ownsUDP(os.Getpid(), host, port); err == nil {
				t.Fatal("accepted closed UDP socket")
			}
		})
	}
}

// Opt-in: uses an independently verified official executable, with loopback
// QUIC only. This checks real authentication/lifecycle, not public forwarding.
func TestOfficialInstanceLifecycle(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_HYSTERIA")
	if binary == "" {
		t.Skip("PANEL_AGENT_TEST_HYSTERIA not set")
	}
	port := func(network string) int {
		t.Helper()
		if network == "udp" {
			p, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			return p.LocalAddr().(*net.UDPAddr).Port
		}
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	opts := serverOptions(t)
	opts.InstanceDir = t.TempDir()
	if err := os.Chmod(opts.InstanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	opts.ListenPort = port("udp")
	opts.AuthAddress = fmt.Sprintf("127.0.0.1:%d", port("tcp"))
	for {
		opts.StatsAddress = fmt.Sprintf("127.0.0.1:%d", port("tcp"))
		if opts.StatsAddress != opts.AuthAddress {
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	instance, err := StartInstance(ctx, binary, opts, []Account{{ID: "user-node", Secret: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := instance.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	if _, err := StartInstance(ctx, binary, opts, nil); err == nil {
		t.Fatal("duplicate instance admitted")
	}
	api, err := NewAPI(opts.StatsAddress, opts.StatsSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	startClient := func(secret string) *exec.Cmd {
		t.Helper()
		config := map[string]any{"server": fmt.Sprintf("127.0.0.1:%d", opts.ListenPort), "auth": secret, "tls": map[string]any{"sni": opts.ServerName, "ca": filepath.Join(opts.InstanceDir, "server.crt")}, "socks5": map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", port("tcp"))}}
		data, _ := json.Marshal(config)
		p := filepath.Join(t.TempDir(), "client.json")
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, binary, "client", "--config", p)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
		return command
	}
	startClient(testSecret)
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, err := api.request(ctx, "GET", "/online", nil)
		var online map[string]int
		if err == nil && json.Unmarshal(body, &online) == nil && online["user-node"] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("official client did not authenticate over IPv4 QUIC")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("official client authenticated over IPv4 QUIC with certificate verification")
	if err := instance.ReplaceAccounts(ctx, nil); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("revocation: %v", err)
	}
	select {
	case <-instance.done:
	default:
		t.Fatal("revocation returned before child exit")
	}
	if _, err := instance.Snapshot(ctx); err == nil {
		t.Fatal("stopped instance still sampled")
	}
	// Reuse the same directory only after the owned child and lock are gone.
	expiring, err := StartInstance(ctx, binary, opts, []Account{{ID: "user-node", Secret: strings.Repeat("c", 32), ExpiresAt: time.Now().Add(2 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	defer expiring.Stop(ctx)
	select {
	case <-expiring.done:
		t.Log("expiry stopped the official child")
	case <-time.After(4 * time.Second):
		t.Fatal("expiry left child running")
	}
	orphan, err := StartInstance(ctx, binary, opts, []Account{{ID: "user-node", Secret: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = orphan.Stop(cleanup)
	})
	otherDir := t.TempDir()
	if err := os.Chmod(otherDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := StopOrphans(ctx, binary, otherDir); err != nil {
		t.Fatal(err)
	}
	if _, err := orphan.Snapshot(ctx); err != nil {
		t.Fatal("recovery stopped a different instance")
	}
	if err := StopOrphans(ctx, binary, opts.InstanceDir); err != nil {
		t.Fatal(err)
	}
	if err := orphan.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("orphan recovery stopped only the exact managed config")
}

func TestStartRejectsExecutableSymlinkWithoutLaunching(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "hysteria")
	if err := os.WriteFile(binary, []byte("must never execute"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if _, err := StartInstance(context.Background(), link, ServerOptions{}, nil); err == nil {
		t.Fatal("accepted executable symlink")
	}
}
