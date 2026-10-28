package agent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestHysteriaSessionLedgerFailureStopsOfficialCore(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_HYSTERIA")
	if binary == "" {
		t.Skip("PANEL_AGENT_TEST_HYSTERIA not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	machine, _ := security.NewUUID()
	user, _ := security.NewUUID()
	nodeID, _ := security.NewUUID()
	secret, _ := security.NewUUID()
	book, err := ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	pair := certificateServer.TLS.Certificates[0]
	leaf := certificateServer.Certificate()
	certificateServer.Close()
	privateKey, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
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
	node := protocol.DesiredNode{ID: nodeID, Enabled: true, ListenHost: "127.0.0.1", ListenPort: port("udp"), Credentials: []protocol.ProxyCredential{{UserID: user, UUID: secret, Email: "session-user"}}}
	policies := []period.Policy{}
	for _, scope := range []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: nodeID}, {Kind: "machine", MachineID: machine}} {
		p, err := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, p)
	}
	options := hysteria.ServerOptions{ServerName: leaf.DNSNames[0], CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})), AuthAddress: fmt.Sprintf("127.0.0.1:%d", port("tcp")), AuthRouteToken: "0123456789abcdef0123456789abcdef", StatsSecret: "abcdef0123456789abcdef0123456789"}
	for {
		options.StatsAddress = fmt.Sprintf("127.0.0.1:%d", port("tcp"))
		if options.StatsAddress != options.AuthAddress {
			break
		}
	}
	session, err := startHysteriaSession(ctx, dir, book, binary, node, policies, protocol.QuotaRestrictions{}, options, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := session.instance.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	if err := session.sample(ctx); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.sample(ctx); err == nil {
		t.Fatal("ledger failure accepted")
	}
	if _, err := session.instance.Snapshot(ctx); err == nil {
		t.Fatal("unmetered instance left running")
	}
	if session.source.state.Pending == nil {
		t.Fatal("failed ledger observation lost")
	}
	t.Log("ledger failure stopped official child and retained pending observation")
}
