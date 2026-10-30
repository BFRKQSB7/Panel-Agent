package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestRealCoreDurableLedgerAndLostACK(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_XRAY")
	if binary == "" {
		t.Skip("requires explicitly authorized pinned Core")
	}
	// Preserve production Freedom rules while giving this test a deterministic
	// public destination. The namespace affects only the child test process.
	if os.Getenv("PANEL_AGENT_TEST_PUBLIC_ORIGIN") == "" {
		child := exec.Command("unshare", "--net", "sh", "-c", `ip link set lo up && ip addr add 1.1.1.1/32 dev lo && exec "$@"`, "sh", os.Args[0], "-test.run=^TestRealCoreDurableLedgerAndLostACK$", "-test.v")
		child.Env = append(os.Environ(), "PANEL_AGENT_TEST_PUBLIC_ORIGIN=1.1.1.1")
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated accounting test: %v\n%s", err, output)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "controlled Reality target") }))
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}}
	target.EnableHTTP2 = true
	target.StartTLS()
	defer target.Close()
	payload := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "Example Domain controlled accounting payload")
	}))
	originHost := os.Getenv("PANEL_AGENT_TEST_PUBLIC_ORIGIN")
	listener, err := net.Listen("tcp", net.JoinHostPort(originHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	payload.Listener.Close()
	payload.Listener = listener
	payload.Start()
	defer payload.Close()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	machine, _ := security.NewUUID()
	user, _ := security.NewUUID()
	node, _ := security.NewUUID()
	uuid, _ := security.NewUUID()
	authority, _ := security.NewUUID()
	installation, _ := security.NewUUID()
	book, err := ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { book.Close() }()
	m, err := NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	m.ledger = book
	t.Cleanup(func() {
		processes, _ := m.ownedProcesses()
		for _, p := range processes {
			_ = stopCoreProcess(context.Background(), p, binary)
		}
	})
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	label := "u-" + user + "-n-" + node
	desired := protocol.DesiredState{Nodes: []protocol.DesiredNode{{ID: node, Name: "ledger-real", PublicHost: "127.0.0.1", PublicPort: 19449, ListenHost: "127.0.0.1", ListenPort: 19449, Enabled: true, Reality: protocol.RealityConfig{ServerName: "example.com", Target: target.Listener.Addr().String(), PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: uuid, Email: label}}}}}
	for _, scope := range []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: node}, {Kind: "machine", MachineID: machine}} {
		p, err := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
		if err != nil {
			t.Fatal(err)
		}
		desired.PeriodPolicies = append(desired.PeriodPolicies, p)
	}
	apply := func(revision uint64) {
		t.Helper()
		digest, _ := protocol.DigestDesiredState(desired)
		if err := m.Apply(ctx, authority, revision, digest, desired); err != nil {
			t.Fatal(err)
		}
	}
	apply(1)
	clientConfig := map[string]any{"log": map[string]any{"loglevel": "debug"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": 19450, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}}}, "outbounds": []any{map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": 19449, "users": []any{map[string]any{"id": uuid, "encryption": "none"}}}}}, "streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{"serverName": "example.com", "fingerprint": "chrome", "publicKey": desired.Nodes[0].Reality.PublicKey, "shortId": "0123456789abcdef"}}}}}
	data, _ := json.Marshal(clientConfig)
	path := filepath.Join(dir, "reality-client.json")
	if err := storage.WritePrivateAtomic(path, data); err != nil {
		t.Fatal(err)
	}
	clientLog := filepath.Join(dir, "reality-client.log")
	clientProcess, err := startCoreCommand(binary, path, clientLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopCoreProcess(context.Background(), clientProcess, binary) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:19450", 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Reality client not ready")
		}
		time.Sleep(30 * time.Millisecond)
	}
	dialer, err := proxy.SOCKS5("tcp", "127.0.0.1:19450", nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.(proxy.ContextDialer).DialContext(ctx, network, address)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	requests := 0
	traffic := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			req, _ := http.NewRequestWithContext(ctx, "GET", payload.URL, nil)
			res, err := httpClient.Do(req)
			if err != nil {
				for _, p := range []string{clientLog, m.logPath} {
					if b, e := os.ReadFile(p); e == nil {
						t.Logf("%s: %s", filepath.Base(p), b)
					}
				}
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode != 200 || !bytes.Contains(body, []byte("Example Domain")) {
				t.Fatal("real proxy response failed", err)
			}
			requests++
		}
	}
	readRaw := func() protocol.Totals {
		t.Helper()
		probe, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		stats, err := xray.DialStats(probe, "127.0.0.1:10085")
		if err != nil {
			t.Fatal(err)
		}
		defer stats.Close()
		up, err := stats.QueryCounter(probe, "user>>>"+label+">>>traffic>>>uplink")
		if err != nil {
			t.Fatal(err)
		}
		down, err := stats.QueryCounter(probe, "user>>>"+label+">>>traffic>>>downlink")
		if err != nil {
			t.Fatal(err)
		}
		return protocol.Totals{Uplink: quantity.Bytes(up), Downlink: quantity.Bytes(down)}
	}
	// No server ingest SQL: this deterministic receiver checks sender retries.
	var mu sync.Mutex
	committed := map[protocol.SegmentKey]protocol.TrafficSegment{}
	replays := 0
	dropped := false
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/agent/traffic/recover" {
			var req protocol.TrafficRecoveryRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: req.AuthorityEpoch, MachineID: req.MachineID, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true, Receipt: req.Receipt})
			return
		}
		var req protocol.TrafficRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		a := protocol.TrafficACK{Version: 1, RequestID: req.RequestID, AuthorityEpoch: req.AuthorityEpoch, MachineID: req.MachineID, LedgerEpoch: req.LedgerEpoch}
		seqs := []uint64{}
		for i, s := range req.Segments {
			if s.Validate() != nil || !s.Finalized {
				w.WriteHeader(400)
				return
			}
			if old, exists := committed[s.Key]; exists {
				if protocol.ValidateCumulativeUpdate(old, s) != nil {
					w.WriteHeader(409)
					return
				}
				replays++
			}
			committed[s.Key] = s
			totals := s.Totals
			a.Outcomes = append(a.Outcomes, protocol.TrafficOutcome{Row: i, Key: s.Key, Status: "final", Committed: &totals, CloseSeq: s.CloseSeq})
		}
		for _, s := range committed {
			seqs = append(seqs, s.CloseSeq)
		}
		prefix, _ := protocol.AdvanceClosedThrough(0, seqs)
		receipt, err := security.SignClosedReceipt(bytes.Repeat([]byte{9}, 32), security.ClosedClaims{Version: 1, InstallationID: installation, MachineID: machine, LedgerEpoch: req.LedgerEpoch, ClosedThrough: prefix})
		if err != nil {
			w.WriteHeader(500)
			return
		}
		a.Receipt = &receipt
		if !dropped {
			dropped = true
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(a)
	}))
	defer receiver.Close()
	identityStore, err := openIdentityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := security.NewToken(32)
	sender, err := NewClient(identityStore, Identity{PanelURL: receiver.URL, MachineID: machine, MachineSecret: secret, AuthorityEpoch: authority}, "")
	if err != nil {
		t.Fatal(err)
	}
	sender.httpClient = receiver.Client()
	sender.ledger = book
	inspect := func() []protocol.TrafficSegment {
		t.Helper()
		req, err := book.RecoveryRequest(ctx, authority)
		if err != nil {
			t.Fatal(err)
		}
		if err = book.ConfirmRecovery(ctx, req, protocol.TrafficRecoveryResponse{Version: 1, RequestID: req.RequestID, AuthorityEpoch: authority, MachineID: machine, LedgerEpoch: req.LedgerEpoch, InstallationID: installation, Resume: true, Receipt: req.Receipt}); err != nil {
			t.Fatal(err)
		}
		report, err := book.Pending(ctx, authority, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return report.Segments
	}
	traffic(1)
	if err = m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	raw1 := readRaw()
	segments := inspect()
	if len(segments) != 1 || segments[0].Totals != raw1 || raw1.Uplink == 0 || raw1.Downlink == 0 {
		t.Fatal("actual Stats did not enter durable ledger", segments, raw1)
	}
	firstK := segments[0].Key
	generation, instance := m.state.Generation, m.state.InstanceID
	book.Close()
	book, err = ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	sender.ledger = book
	m, err = NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	m.ledger = book
	if err = m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err = m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	segments = inspect()
	if m.state.Generation != generation || m.state.InstanceID != instance || segments[0].Key != firstK || segments[0].Totals != raw1 {
		t.Fatal("Agent-only restart changed identity or double billed")
	}
	// Keep the sampler running independently of any Panel request. Actual proxy
	// bytes must land without a control sync or a successful receiver connection.
	sender.core = m
	func() {
		measureCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); sender.runSampler(measureCtx) }()
		defer func() { stop(); <-done }()
		traffic(1)
		deadline := time.Now().Add(8 * time.Second)
		for {
			segments = inspect()
			if len(segments) == 1 && segments[0].Totals.Uplink > raw1.Uplink {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("offline continuous sampler did not persist traffic")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	raw1 = readRaw()
	segments = inspect()
	if len(segments) != 1 || segments[0].Totals != raw1 {
		t.Fatal("offline sampler changed totals")
	}
	// Interrupt the Core switch after its final snapshot has committed, while
	// the old Core still runs. Recovery consumes the same boundary exactly once.
	if _, err = m.captureBoundary(ctx, false); err != nil {
		t.Fatal(err)
	}
	nextJournal := m.state
	nextJournal.Boundary = ""
	if err = m.save(nextJournal); err != nil {
		t.Fatal(err)
	}
	m, err = NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	m.ledger = book
	sender.core = m
	if err = m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err = m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	segments = inspect()
	if len(segments) != 2 || !segments[0].Finalized || segments[0].Key != firstK || segments[0].Totals != raw1 || segments[1].Totals.Uplink != 0 || segments[1].Totals.Downlink != 0 {
		t.Fatal("interrupted boundary duplicated traffic")
	}
	if err = stopCoreProcess(ctx, *m.state.Process, binary); err != nil {
		t.Fatal(err)
	}
	if err = m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	traffic(3)
	raw2 := readRaw()
	if raw2.Uplink <= raw1.Uplink || raw2.Downlink <= raw1.Downlink {
		t.Fatal("new Core counter did not exceed old watermark")
	}
	if err = m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if m.state.Generation <= generation || m.state.InstanceID == instance {
		t.Fatal("Core restart reused identity")
	}
	traffic(1)
	finalRaw := readRaw()
	desired.Nodes[0].Enabled = false
	apply(2)
	segments = inspect()
	var total protocol.Totals
	for _, s := range segments {
		if !s.Finalized || s.Stream.UserID != user || s.Stream.EntryNodeID != node || s.Key.LedgerEpoch != firstK.LedgerEpoch {
			t.Fatal("deletion lost old attribution")
		}
		total.Uplink, err = quantity.Add(total.Uplink, s.Totals.Uplink)
		if err != nil {
			t.Fatal(err)
		}
		total.Downlink, err = quantity.Add(total.Downlink, s.Totals.Downlink)
		if err != nil {
			t.Fatal(err)
		}
	}
	if total.Uplink != raw1.Uplink+finalRaw.Uplink || total.Downlink != raw1.Downlink+finalRaw.Downlink {
		t.Fatal("final sample before deletion lost bytes", total, raw1, finalRaw)
	}
	if err = sender.syncTraffic(ctx); err == nil {
		t.Fatal("lost ACK injection not exercised")
	}
	before, err := book.Diagnostic(ctx)
	if err != nil || before.Pending == 0 {
		t.Fatal("lost ACK removed pending", err)
	}
	if err = sender.syncTraffic(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := book.Diagnostic(ctx)
	if err != nil || after.Pending != 0 || after.Metadata.Receipt == nil {
		t.Fatal("ACK retry/GC failed", err)
	}
	mu.Lock()
	count, duplicates := len(committed), replays
	mu.Unlock()
	if count != len(segments) || duplicates < count {
		t.Fatal("retry fixture did not see stable K")
	}
	t.Logf("real_requests=%d; raw_before_restart=%v; raw_after_restart=%v; final_sum=%v; frozen_segments=%d; duplicate_reports=%d; closed_through=%d; physical_stop_tail=unbounded", requests, raw1, raw2, total, count, duplicates, after.Metadata.Receipt.ClosedThrough)
}
