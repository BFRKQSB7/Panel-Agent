package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

// The historical floor is changed in the test process only. Both ELF binaries
// are unmodified official releases, checked by the runner before execution.
func TestRealHistoricalCoreUpgrade(t *testing.T) {
	oldBinary, newBinary := os.Getenv("PANEL_AGENT_TEST_XRAY_OLD"), os.Getenv("PANEL_AGENT_TEST_XRAY")
	if oldBinary == "" || newBinary == "" {
		t.Skip("requires explicit official historical and current Linux Xray binaries")
	}
	previousFloor := minimumSupportedCoreVersion
	minimumSupportedCoreVersion = "26.9.30"
	t.Cleanup(func() { minimumSupportedCoreVersion = previousFloor })
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	oldVersion, err := readCoreVersion(ctx, oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := readCoreVersion(ctx, newBinary)
	if err != nil || compareCoreVersions(newVersion, oldVersion) <= 0 {
		t.Fatal("distinct increasing official versions required", err)
	}
	hashFile := func(path string) string {
		t.Helper()
		f, e := os.Open(path)
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		h := sha256.New()
		if _, e := io.Copy(h, f); e != nil {
			t.Fatal(e)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	oldSHA, newSHA := hashFile(oldBinary), hashFile(newBinary)

	// The actual Reality client and HTTP target stay alive across each cutover.
	listener, err := net.Listen("tcp4", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "d15-real-proxy") }), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	for _, scenario := range []string{"success", "start-failure", "crash-prepared", "crash-stopped", "crash-swapped"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "xray")
			if err := copyUpgradeTestBinary(oldBinary, binary); err != nil {
				t.Fatal(err)
			}
			m, err := NewCoreManager(dir, binary, true)
			if err != nil {
				t.Fatal(err)
			}
			machine, _ := security.NewUUID()
			book, err := ledger.Open(dir, machine)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = book.Close() }()
			m.ledger = book
			t.Cleanup(func() {
				processes, e := m.ownedProcesses()
				if e != nil {
					t.Error(e)
					return
				}
				for _, process := range processes {
					if e := stopCoreProcess(context.Background(), process, binary); e != nil {
						t.Error(e)
					}
				}
			})
			user, _ := security.NewUUID()
			node, _ := security.NewUUID()
			uuid, _ := security.NewUUID()
			authority, _ := security.NewUUID()
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			r := protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}
			desired := protocol.EmptyDesiredState()
			desired.Nodes = []protocol.DesiredNode{{ID: node, Name: "D15 isolated", PublicHost: "127.0.0.1", PublicPort: 19449, ListenHost: "127.0.0.1", ListenPort: 19449, Enabled: true, Reality: r, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: uuid, Email: "u-" + user + "-n-" + node, Development: true}}}}
			for _, scope := range []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: node}, {Kind: "machine", MachineID: machine}} {
				p, e := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
				if e != nil {
					t.Fatal(e)
				}
				desired.PeriodPolicies = append(desired.PeriodPolicies, p)
			}
			digest, err := protocol.DigestDesiredState(desired)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Apply(ctx, authority, 1, digest, desired); err != nil {
				t.Fatal(err)
			}
			originalConfig, generation := m.state.Current.ConfigHash, m.state.Generation
			clientConfig := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": 19450, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}}}, "outbounds": []any{map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": 19449, "users": []any{map[string]any{"id": uuid, "encryption": "none"}}}}}, "streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{"serverName": r.ServerName, "fingerprint": "chrome", "publicKey": r.PublicKey, "shortId": r.ShortID}}}}}
			data, err := json.Marshal(clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			clientPath := filepath.Join(dir, "client.json")
			if err := storage.WritePrivateAtomic(clientPath, data); err != nil {
				t.Fatal(err)
			}
			client, err := startCoreCommand(newBinary, clientPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if e := stopCoreProcess(context.Background(), client, newBinary); e != nil {
					t.Error(e)
				}
			}()
			dialer, err := proxy.SOCKS5("tcp", "127.0.0.1:19450", nil, &net.Dialer{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{DisableKeepAlives: true, DialContext: dialer.(proxy.ContextDialer).DialContext}
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: transport, Timeout: 8 * time.Second}
			traffic := func() {
				t.Helper()
				var last error
				for attempt := 0; attempt < 5; attempt++ {
					response, e := httpClient.Get("http://127.0.0.1:18080/")
					if e == nil {
						body, e := io.ReadAll(response.Body)
						response.Body.Close()
						if e == nil && string(body) == "d15-real-proxy" {
							return
						}
						last = errors.New("incorrect proxied payload")
					} else {
						last = e
					}
					time.Sleep(100 * time.Millisecond)
				}
				t.Fatal("Reality proxy failed", last)
			}
			traffic()
			if err := m.Sample(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := book.Diagnostic(ctx)
			if err != nil || len(before.PendingPreview) == 0 {
				t.Fatal("missing pre-upgrade unacknowledged traffic", err)
			}
			intent, _ := security.NewUUID()
			clientAgent := &Client{core: m}
			stage := func() (string, error) {
				packagePath := os.Getenv("PANEL_AGENT_TEST_XRAY_PACKAGE")
				if packagePath == "" {
					packagePath = newBinary
				}
				contents, e := os.ReadFile(packagePath)
				if e != nil {
					return "", e
				}
				sum := sha256.Sum256(contents)
				return clientAgent.stageUpgradePackage(intent, contents, hex.EncodeToString(sum[:]))
			}
			candidate, err := stage()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.UpgradeBinary(ctx, intent, newVersion, candidate, oldSHA); err == nil || m.state.Generation != generation {
				t.Fatal("checksum failure changed healthy Core")
			}
			if scenario == "start-failure" {
				m.upgradeCheckpoint = func(phase string) {
					if phase == "swapped" {
						if e := os.Chmod(binary, 0o600); e != nil {
							t.Fatal(e)
						}
					}
				}
			}
			if len(scenario) > 6 && scenario[:6] == "crash-" {
				if err := book.Close(); err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoricalCoreUpgradeCrashWorker$", "-test.v")
				command.Env = append(os.Environ(), "PANEL_AGENT_D15_CRASH_ROOT="+dir, "PANEL_AGENT_D15_CRASH_PHASE="+scenario[6:], "PANEL_AGENT_D15_INTENT="+intent, "PANEL_AGENT_D15_MACHINE="+machine, "PANEL_AGENT_D15_TARGET="+newVersion, "PANEL_AGENT_D15_SHA="+newSHA)
				output, e := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("crash worker: %v %s", e, output)
				}
				m, err = NewCoreManager(dir, binary, true)
				if err != nil {
					t.Fatal(err)
				}
				book, err = ledger.Open(dir, machine)
				if err != nil {
					t.Fatal(err)
				}
				m.ledger = book
				if err := m.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				outcome, e := m.UpgradeBinary(ctx, intent, newVersion, candidate, newSHA)
				if scenario == "success" {
					if e != nil || outcome.ActualVersion != newVersion || outcome.RolledBack || m.state.Generation <= generation || hashFile(binary) != newSHA {
						t.Fatalf("upgrade not committed: %+v %v", outcome, e)
					}
					// A repeated Intent stages the same package but must not restart again.
					candidate, err = stage()
					if err != nil {
						t.Fatal(err)
					}
					g := m.state.Generation
					if _, err := m.UpgradeBinary(ctx, intent, newVersion, candidate, newSHA); err != nil || m.state.Generation != g {
						t.Fatal("repeated upgrade was not idempotent", err)
					}
				} else if e == nil || !outcome.RolledBack || hashFile(binary) != oldSHA {
					t.Fatalf("startup failure did not restore old ELF: %+v %v", outcome, e)
				}
			}
			if scenario != "success" && (hashFile(binary) != oldSHA || m.CurrentVersion() != oldVersion) {
				t.Fatal("crash/rollback did not restore exact historical binary")
			}
			if m.state.Current.ConfigHash != originalConfig {
				t.Fatal("configuration changed during upgrade")
			}
			traffic()
			if err := m.Sample(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := book.Diagnostic(ctx)
			if err != nil || after.Metadata.Epoch != before.Metadata.Epoch {
				t.Fatal("ledger epoch changed", err)
			}
			for _, prior := range before.PendingPreview {
				found := false
				for _, next := range after.PendingPreview {
					if next.Key == prior.Key {
						found = true
						if next.Totals.Uplink < prior.Totals.Uplink || next.Totals.Downlink < prior.Totals.Downlink {
							t.Fatal("unacknowledged totals lost")
						}
					}
				}
				if !found {
					t.Fatal("unacknowledged segment lost")
				}
			}
			t.Logf("old=%s target=%s actual=%s generations=%d->%d ledger_epoch=%s retained_segments=%d proxy=ok", oldVersion, newVersion, m.CurrentVersion(), generation, m.state.Generation, after.Metadata.Epoch, len(before.PendingPreview))
		})
	}
}

func copyUpgradeTestBinary(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(err, closeErr)
}

func TestHistoricalCoreUpgradeCrashWorker(t *testing.T) {
	dir := os.Getenv("PANEL_AGENT_D15_CRASH_ROOT")
	if dir == "" {
		t.Skip("subprocess only")
	}
	minimumSupportedCoreVersion = "26.9.30"
	m, err := NewCoreManager(dir, filepath.Join(dir, "xray"), true)
	if err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(dir, os.Getenv("PANEL_AGENT_D15_MACHINE"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	m.ledger = book
	m.upgradeCheckpoint = func(phase string) {
		if phase == os.Getenv("PANEL_AGENT_D15_CRASH_PHASE") {
			os.Exit(77)
		}
	}
	intent := os.Getenv("PANEL_AGENT_D15_INTENT")
	_, err = m.UpgradeBinary(context.Background(), intent, os.Getenv("PANEL_AGENT_D15_TARGET"), filepath.Join(m.dir, coreUpgradeDirName, intent, "xray"), os.Getenv("PANEL_AGENT_D15_SHA"))
	t.Fatalf("checkpoint not reached: %v", err)
}

func TestRealCoreUpgradeLowDisk(t *testing.T) {
	limitedRoot := os.Getenv("PANEL_AGENT_TEST_LOW_DISK_ROOT")
	oldBinary, newBinary := os.Getenv("PANEL_AGENT_TEST_XRAY_OLD"), os.Getenv("PANEL_AGENT_TEST_XRAY")
	if limitedRoot == "" || oldBinary == "" || newBinary == "" {
		t.Skip("requires explicit small mounted filesystem and real Cores")
	}
	previousFloor := minimumSupportedCoreVersion
	minimumSupportedCoreVersion = "26.9.30"
	t.Cleanup(func() { minimumSupportedCoreVersion = previousFloor })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir, err := os.MkdirTemp(limitedRoot, "d15-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(binDir) })
	binary := filepath.Join(binDir, "xray")
	if err := copyUpgradeTestBinary(oldBinary, binary); err != nil {
		t.Fatal(err)
	}
	m, err := NewCoreManager(dir, binary, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		processes, _ := m.ownedProcesses()
		for _, p := range processes {
			if e := stopCoreProcess(context.Background(), p, binary); e != nil {
				t.Error(e)
			}
		}
	})
	desired := protocol.EmptyDesiredState()
	digest, err := protocol.DigestDesiredState(desired)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := security.NewUUID()
	if err := m.Apply(ctx, authority, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	process, generation := *m.state.Process, m.state.Generation
	intent, _ := security.NewUUID()
	candidate := filepath.Join(m.dir, coreUpgradeDirName, intent, "xray")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyUpgradeTestBinary(newBinary, candidate); err != nil {
		t.Fatal(err)
	}
	available, err := availableDiskBytes(binDir)
	if err != nil || available >= coreUpgradeHeadroom {
		t.Fatal("fixture is not genuinely disk-limited", available, err)
	}
	_, err = m.UpgradeBinary(ctx, intent, "26.10.21", candidate, candidateDigest(candidate))
	var upgradeErr *CoreUpgradeError
	if !errors.As(err, &upgradeErr) || upgradeErr.Code != protocol.UpgradeErrorLowDisk || *m.state.Process != process || m.state.Generation != generation {
		t.Fatal("disk pressure did not preserve current Core", err)
	}
	if err := m.health(ctx, process, m.state.Current, false); err != nil {
		t.Fatal(err)
	}
	downloadClient := &Client{core: &CoreManager{dir: binDir}}
	if _, err := downloadClient.downloadUpgradePackage(ctx, protocol.UpgradeIntent{Source: protocol.UpgradeSourceOfficialXray, PackageSize: 15100223, TargetVersion: "26.10.21"}); err == nil {
		t.Fatal("download started on insufficient disk")
	}
	t.Logf("actual_available_bytes=%d low_disk=refused running_core=unchanged", available)
}

type upgradeTestTransport func(*http.Request) (*http.Response, error)

func (f upgradeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpgradeDownloadRejectsUntrustedBodies(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, scenario := range []string{"short", "extra", "checksum", "status", "transport"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			client := &Client{core: &CoreManager{dir: dir}}
			intent := protocol.UpgradeIntent{ID: "11111111-1111-4111-8111-111111111111", Source: protocol.UpgradeSourceOfficialXray, TargetVersion: "26.10.21", PackageSize: 4, SHA256: hex.EncodeToString(packageDigest([]byte("good")))}
			http.DefaultTransport = upgradeTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme != "https" || r.URL.Host != "github.com" || !strings.HasPrefix(r.URL.Path, "/XTLS/Xray-core/releases/download/v26.10.21/") {
					t.Fatal("uncontrolled release URL", r.URL)
				}
				if scenario == "transport" {
					return nil, io.ErrUnexpectedEOF
				}
				body, status := "bad", 200
				switch scenario {
				case "extra":
					body = "extra"
				case "checksum":
					body = "evil"
				case "status":
					body, status = "good", 503
				}
				return &http.Response{StatusCode: status, ContentLength: -1, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			if _, err := client.downloadUpgradePackage(context.Background(), intent); err == nil {
				t.Fatal("invalid download accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, coreUpgradeDirName, intent.ID)); !os.IsNotExist(err) {
				t.Fatal("download failure staged data", err)
			}
		})
	}
}
