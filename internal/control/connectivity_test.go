package control

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

type testIPResolver struct {
	ips   []net.IP
	err   error
	calls int
}

func (r *testIPResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	r.calls++
	return r.ips, r.err
}

func testProbeNode(host string, port int) storage.NodeRecord {
	return storage.NodeRecord{NodeID: "00000000-0000-4000-8000-000000000001", NodeInput: storage.NodeInput{PublicHost: host, PublicPort: port}}
}

func TestNodeGuideUsesCurrentIPv4IPv6AndDomainValues(t *testing.T) {
	machine := protocol.MachineView{}
	v4 := makeNodeGuide("00000000-0000-4000-8000-000000000010", storage.NodeRecord{NodeID: "node-v4", NodeInput: storage.NodeInput{PublicHost: "198.51.100.20", PublicPort: 45001, ListenHost: "10.0.0.8", ListenPort: 18443, ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"}, PublicKey: "public-v4", ShortID: "0123456789abcdef"}, machine)
	if !strings.Contains(v4.NAT[0].Text, "198.51.100.20:45001 -> 本机 10.0.0.8:18443") || !strings.Contains(v4.DNS[0].Text, "A") || strings.Contains(v4.DNS[0].Text, "AAAA 记录") || !strings.Contains(v4.Protocol[2].Text, "不需要上传普通 TLS 证书") {
		t.Fatalf("IPv4 guide = %+v", v4)
	}
	v6 := makeNodeGuide("machine-v6", storage.NodeRecord{NodeID: "node-v6", NodeInput: storage.NodeInput{PublicHost: "2001:db8::20", PublicPort: 45002, ListenHost: "::", ListenPort: 18444, ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"}, PublicKey: "public-v6", ShortID: "fedcba9876543210"}, machine)
	if !strings.Contains(v6.NAT[0].Text, "[2001:db8::20]:45002") || !strings.Contains(v6.DNS[0].Text, "AAAA") {
		t.Fatalf("IPv6 guide = %+v", v6)
	}
	domain := makeNodeGuide("machine-domain", storage.NodeRecord{NodeID: "node-domain", NodeInput: storage.NodeInput{PublicHost: "edge.example.test", PublicPort: 45123, ListenHost: "0.0.0.0", ListenPort: 19443, ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"}, PublicKey: "public-domain", ShortID: "abcdef0123456789"}, machine)
	if len(domain.DNS) != 3 || !strings.Contains(domain.DNS[0].Text, "A edge.example.test") || !strings.Contains(domain.DNS[1].Text, "AAAA edge.example.test") || domain.LocalProbe {
		t.Fatalf("domain guide = %+v", domain)
	}
}

func TestConnectivityProbeSeparatesDNSAndTCPAndBindsOneAddress(t *testing.T) {
	resolver := &testIPResolver{ips: []net.IP{net.ParseIP("198.51.100.31")}}
	var dialed string
	result := runConnectivityProbeWith(context.Background(), testProbeNode("edge.example.test", 45123), connectivityModePublic, resolver, func(_ context.Context, network, address string) (net.Conn, error) {
		dialed = network + " " + address
		return nil, errors.New("connection refused")
	}, time.Unix(100, 0))
	if resolver.calls != 1 || result.DNS.Status != "passed" || result.TCP.Status != "failed" || result.OK || dialed != "tcp 198.51.100.31:45123" || result.ResolvedIP != "198.51.100.31" {
		t.Fatalf("DNS/TCP result = %+v, calls=%d, dialed=%q", result, resolver.calls, dialed)
	}
}

func TestConnectivityProbeRejectsPrivateMappedAndRebindingTargets(t *testing.T) {
	cases := []struct {
		name string
		host string
		ips  []net.IP
	}{
		{name: "literal private", host: "192.168.1.10"},
		{name: "mapped private", host: "::ffff:10.0.0.7"},
		{name: "dns rebinding", host: "edge.example.test", ips: []net.IP{net.ParseIP("198.51.100.40"), net.ParseIP("10.0.0.8")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &testIPResolver{ips: tc.ips}
			dialed := false
			result := runConnectivityProbeWith(context.Background(), testProbeNode(tc.host, 45123), connectivityModePublic, resolver, func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, nil
			}, time.Now())
			if result.DNS.Status != "rejected" || result.TCP.Status != "not_checked" || dialed {
				t.Fatalf("unsafe target was not rejected: %+v, dialed=%v", result, dialed)
			}
		})
	}
	resolver := &testIPResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	result := runConnectivityProbeWith(context.Background(), testProbeNode("localhost", 45123), connectivityModeLocal, resolver, func(context.Context, string, string) (net.Conn, error) { return nil, nil }, time.Now())
	if result.DNS.Status != "rejected" || resolver.calls != 0 {
		t.Fatalf("local DNS target bypassed explicit loopback branch: %+v, calls=%d", result, resolver.calls)
	}
}

func TestConnectivityProbeLocalLoopbackCanReachTCPListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	result := runConnectivityProbe(context.Background(), testProbeNode("127.0.0.1", port), connectivityModeLocal)
	if !result.OK || result.DNS.Status != "passed" || result.TCP.Status != "passed" || result.ResolvedIP != "127.0.0.1" {
		t.Fatalf("local loopback result = %+v", result)
	}
}
