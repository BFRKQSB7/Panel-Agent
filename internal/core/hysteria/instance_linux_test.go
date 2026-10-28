package hysteria

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
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
