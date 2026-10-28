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
	reopened, err := ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := recoverHysteriaSessions(ctx, dir, reopened, binary); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.source.path); !os.IsNotExist(err) {
		t.Fatal("recovered journal not retired")
	}
	diagnostic, err := reopened.Diagnostic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.Sources != 0 {
		t.Fatal("recovered counter source leaked")
	}
	t.Log("restart replayed pending observation and retired only the stopped source")
	// Exercise the same manager used by Client: stable offline recovery, status
	// generation continuity, revocation and interrupted-change fencing.
	node.PublicHost, node.PublicPort, node.Name = "edge.example.com", 45001, "official-hy2"
	node.Extension = &protocol.ExtensionNode{ID: "official-hysteria2", Version: "1.0.0", Kind: "hysteria2_tls", SNI: options.ServerName, CertificatePEM: options.CertificatePEM, PrivateKeyPEM: options.PrivateKeyPEM}
	authority, _ := security.NewUUID()
	desired := protocol.DesiredState{Nodes: []protocol.DesiredNode{node}, PeriodPolicies: policies}
	digest, err := protocol.DigestDesiredState(desired)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newHysteriaManager(ctx, dir, binary, reopened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	if err := manager.Apply(ctx, authority, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	if !manager.Matches(authority, 1, digest) {
		t.Fatal("complete official apply not recognized")
	}
	firstGeneration := manager.Status().Generation
	manager.Close()
	restored, err := newHysteriaManager(ctx, dir, binary, reopened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	if !restored.Matches(authority, 1, digest) || restored.Status().Generation <= firstGeneration {
		t.Fatal("offline restore lost status generation continuity")
	}
	desired.Nodes = []protocol.DesiredNode{node}
	desired.Nodes[0].Credentials = nil
	revokedDigest, err := protocol.DigestDesiredState(desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Apply(ctx, authority, 2, revokedDigest, desired); err != nil {
		t.Fatal(err)
	}
	if len(restored.sessions) != 0 || !restored.Matches(authority, 2, revokedDigest) {
		t.Fatal("revocation did not stop instance")
	}
	desired.Nodes = []protocol.DesiredNode{node}
	pending := restored.record
	pending.Revision = 3
	pending.Digest = digest
	pending.Desired = desired
	pending.Pending = true
	if err := restored.save(pending); err != nil {
		t.Fatal(err)
	}
	restored.Close()
	fenced, err := newHysteriaManager(ctx, dir, binary, reopened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fenced.Close)
	if len(fenced.sessions) != 0 || fenced.Matches(authority, 3, digest) {
		t.Fatal("interrupted permission change resumed without fresh sync")
	}
	if err := fenced.Apply(ctx, authority, 3, digest, desired); err != nil {
		t.Fatal(err)
	}
	if !fenced.Matches(authority, 3, digest) {
		t.Fatal("fresh sync did not resolve interrupted change")
	}
	t.Log("manager applied, restored, revoked and fenced interrupted changes")
}
