package control

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestMachineEnrollmentRecoveryScopeAuthorityRepairAndRevocation(t *testing.T) {
	panelData := t.TempDir()
	if err := os.Chmod(panelData, 0o700); err != nil {
		t.Fatal(err)
	}
	password := []byte("d03-control-test-password")
	hash, err := security.HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(panelData, hash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: panelData, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		_ = server.Close()
	})
	adminClient := httpServer.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	adminClient.Jar = jar
	adminClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	loginControlAdmin(t, adminClient, httpServer.URL, string(password))

	caPath := filepath.Join(t.TempDir(), "panel-test-ca.pem")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: httpServer.Certificate().Raw})
	if err := os.WriteFile(caPath, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}

	csrf := fetchMachinesCSRF(t, adminClient, httpServer.URL)
	joinToken := createJoinTokenThroughAdmin(t, adminClient, httpServer.URL, csrf, "")
	agentDirA := filepath.Join(t.TempDir(), "agent-a")
	machineIDA, err := agent.Pair(context.Background(), agentDirA, httpServer.URL, "机器 A", joinToken, caPath, false)
	if err != nil {
		t.Fatal(err)
	}
	clientA, err := agent.LoadClient(agentDirA, caPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := clientA.Identity().MachineSecret
	oldAuthority := clientA.Identity().AuthorityEpoch
	if clientA.Identity().Revision != 1 || clientA.Identity().Digest == "" || len(clientA.Identity().DesiredState.Nodes) != 0 {
		clientA.Close()
		t.Fatal("Agent did not persist the complete empty desired-state snapshot")
	}

	csrf = fetchMachinesCSRF(t, adminClient, httpServer.URL)
	response := postForm(t, adminClient, httpServer.URL+"/admin/authority/rotate", url.Values{"csrf": {csrf}}, httpServer.URL)
	if response.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		clientA.Close()
		t.Fatalf("authority rotation returned %d: %s", response.StatusCode, body)
	}
	response.Body.Close()
	if err := clientA.SyncOnce(context.Background()); err != nil {
		clientA.Close()
		t.Fatal(err)
	}
	newAuthority := clientA.Identity().AuthorityEpoch
	if newAuthority == oldAuthority || clientA.Identity().AuthorityNonce != "" {
		clientA.Close()
		t.Fatal("Agent did not finish the random authority full-snapshot handshake")
	}
	clientA.Close()

	csrf = fetchMachinesCSRF(t, adminClient, httpServer.URL)
	joinTokenB := createJoinTokenThroughAdmin(t, adminClient, httpServer.URL, csrf, "")
	agentDirB := filepath.Join(t.TempDir(), "agent-b")
	machineIDB, err := agent.Pair(context.Background(), agentDirB, httpServer.URL, "机器 B", joinTokenB, caPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if machineIDB == machineIDA {
		t.Fatal("two registrations received the same machine identity")
	}

	clientA, err = agent.LoadClient(agentDirA, caPath)
	if err != nil {
		t.Fatal(err)
	}
	statusBody, _ := json.Marshal(map[string]any{
		"request_id":      "550e8400-e29b-41d4-a716-446655440011",
		"authority_epoch": clientA.Identity().AuthorityEpoch,
		"revision":        clientA.Identity().Revision,
		"digest":          clientA.Identity().Digest,
		"hostname":        "machine-a",
		"os":              "linux",
		"architecture":    "amd64",
		"cpu_count":       4,
		"memory_bytes":    4096,
		"agent_version":   protocol.AgentVersion,
		"received":        true,
		"applied":         false,
		"apply_error":     protocol.ErrorCoreNotImplemented,
		"machine_id":      machineIDB,
	})
	crossMachineRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/agent/status", strings.NewReader(string(statusBody)))
	crossMachineRequest.Header.Set("Content-Type", "application/json")
	crossMachineRequest.Header.Set("Authorization", "Bearer "+clientA.Identity().MachineSecret)
	crossMachineResponse, err := adminClient.Do(crossMachineRequest)
	if err != nil {
		clientA.Close()
		t.Fatal(err)
	}
	if crossMachineResponse.StatusCode != http.StatusBadRequest {
		crossMachineResponse.Body.Close()
		clientA.Close()
		t.Fatalf("machine token accepted a body-supplied machine_id: HTTP %d", crossMachineResponse.StatusCode)
	}
	crossMachineResponse.Body.Close()

	noCookieClient := &http.Client{
		Transport:     httpServer.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	adminRequest, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/admin/machines", nil)
	adminRequest.Header.Set("Authorization", "Bearer "+clientA.Identity().MachineSecret)
	adminResponse, err := noCookieClient.Do(adminRequest)
	if err != nil {
		clientA.Close()
		t.Fatal(err)
	}
	if adminResponse.StatusCode != http.StatusSeeOther {
		adminResponse.Body.Close()
		clientA.Close()
		t.Fatalf("machine credential accessed admin page: HTTP %d", adminResponse.StatusCode)
	}
	adminResponse.Body.Close()
	clientA.Close()

	machine, err := server.store.GetMachine(context.Background(), machineIDA)
	if err != nil || machine.AuthorityEpoch != newAuthority || machine.ReceivedRevision != 1 || machine.AppliedRevision != 0 || machine.ApplyError != protocol.ErrorCoreNotImplemented {
		t.Fatalf("machine desired/received/applied state is inconsistent: %+v, %v", machine, err)
	}

	csrf = fetchMachinesCSRF(t, adminClient, httpServer.URL)
	repairToken := createJoinTokenThroughAdmin(t, adminClient, httpServer.URL, csrf, machineIDA)
	newMachineID, err := agent.Pair(context.Background(), agentDirA, "", "机器 A 重配", repairToken, caPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if newMachineID != machineIDA {
		t.Fatalf("re-pair changed machine identity: %q != %q", newMachineID, machineIDA)
	}
	oldAuthRequest := protocol.DesiredStateRequest{RequestID: "550e8400-e29b-41d4-a716-446655440012", AuthorityEpoch: newAuthority, Revision: 1, Digest: machine.DesiredDigest}
	oldAuthBody, _ := json.Marshal(oldAuthRequest)
	oldAuthHTTP, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/agent/desired-state", strings.NewReader(string(oldAuthBody)))
	oldAuthHTTP.Header.Set("Content-Type", "application/json")
	oldAuthHTTP.Header.Set("Authorization", "Bearer "+oldSecret)
	oldAuthResponse, err := noCookieClient.Do(oldAuthHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if oldAuthResponse.StatusCode != http.StatusUnauthorized {
		oldAuthResponse.Body.Close()
		t.Fatalf("old machine credential survived re-pair: HTTP %d", oldAuthResponse.StatusCode)
	}
	oldAuthResponse.Body.Close()

	newClientA, err := agent.LoadClient(agentDirA, caPath)
	if err != nil {
		t.Fatal(err)
	}
	newIdentity := newClientA.Identity()
	newSecret := newIdentity.MachineSecret
	newClientA.Close()
	csrf = fetchMachinesCSRF(t, adminClient, httpServer.URL)
	revokeForm := url.Values{"csrf": {csrf}}.Encode()
	revokeRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/machines/"+machineIDA+"/revoke", strings.NewReader(revokeForm))
	revokeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeRequest.Header.Set("Origin", httpServer.URL)
	revokeResponse, err := adminClient.Do(revokeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if revokeResponse.StatusCode != http.StatusSeeOther {
		revokeResponse.Body.Close()
		t.Fatalf("machine revocation returned HTTP %d", revokeResponse.StatusCode)
	}
	revokeResponse.Body.Close()
	revokedDesiredState, _ := json.Marshal(protocol.DesiredStateRequest{
		RequestID: "550e8400-e29b-41d4-a716-446655440013", AuthorityEpoch: newIdentity.AuthorityEpoch,
		Revision: newIdentity.Revision, Digest: newIdentity.Digest,
	})
	revokedRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/agent/desired-state", strings.NewReader(string(revokedDesiredState)))
	revokedRequest.Header.Set("Content-Type", "application/json")
	revokedRequest.Header.Set("Authorization", "Bearer "+newSecret)
	revokedResponse, err := noCookieClient.Do(revokedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if revokedResponse.StatusCode != http.StatusUnauthorized {
		revokedResponse.Body.Close()
		t.Fatalf("revoked machine credential remained valid: HTTP %d", revokedResponse.StatusCode)
	}
	revokedResponse.Body.Close()

	if _, err := server.store.AuthenticateMachine(context.Background(), security.HashToken(newSecret)); err == nil {
		t.Fatal("revoked machine remained authenticated")
	}
}

func TestRegistrationResponseLossRecoveryUsesCandidateSecret(t *testing.T) {
	panelData := t.TempDir()
	if err := os.Chmod(panelData, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordHash, err := security.HashAdminPassword([]byte("d03-recovery-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(panelData, passwordHash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: panelData, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(server.Handler())
	t.Cleanup(func() { tlsServer.Close(); _ = server.Close() })
	joinToken, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registrationID, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.CreateJoinToken(context.Background(), security.HashToken(joinToken), "", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	request := protocol.RegisterRequest{
		RegistrationID: registrationID, JoinToken: joinToken, CandidateSecret: candidate,
		Hostname: "recovery-host", OS: "linux", Architecture: "amd64", CPUCount: 2,
		MemoryBytes: 2048, AgentVersion: protocol.AgentVersion,
	}
	body, _ := json.Marshal(request)
	registerRequest, _ := http.NewRequest(http.MethodPost, tlsServer.URL+"/api/v1/machines/register", strings.NewReader(string(body)))
	registerRequest.Header.Set("Content-Type", "application/json")
	registerResponse, err := tlsServer.Client().Do(registerRequest)
	if err != nil {
		t.Fatal(err)
	}
	registerResponse.Body.Close() // Simulate a lost successful response.
	if registerResponse.StatusCode != http.StatusCreated {
		t.Fatalf("register returned HTTP %d", registerResponse.StatusCode)
	}
	for _, token := range []string{"", "another-candidate-secret"} {
		recoveryBody, _ := json.Marshal(protocol.RecoveryRequest{RegistrationID: registrationID})
		recoveryRequest, _ := http.NewRequest(http.MethodPost, tlsServer.URL+"/api/v1/machines/recover", strings.NewReader(string(recoveryBody)))
		recoveryRequest.Header.Set("Content-Type", "application/json")
		if token != "" {
			other, err := security.NewToken(security.MachineSecretBytes)
			if err != nil {
				t.Fatal(err)
			}
			recoveryRequest.Header.Set("Authorization", "Bearer "+other)
		}
		recoveryResponse, err := tlsServer.Client().Do(recoveryRequest)
		if err != nil {
			t.Fatal(err)
		}
		if recoveryResponse.StatusCode != http.StatusUnauthorized {
			recoveryResponse.Body.Close()
			t.Fatalf("registration ID without candidate secret recovered identity: HTTP %d", recoveryResponse.StatusCode)
		}
		recoveryResponse.Body.Close()
	}
	recoveryBody, _ := json.Marshal(protocol.RecoveryRequest{RegistrationID: registrationID})
	recoveryRequest, _ := http.NewRequest(http.MethodPost, tlsServer.URL+"/api/v1/machines/recover", strings.NewReader(string(recoveryBody)))
	recoveryRequest.Header.Set("Content-Type", "application/json")
	recoveryRequest.Header.Set("Authorization", "Bearer "+candidate)
	recoveryResponse, err := tlsServer.Client().Do(recoveryRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryResponse.Body.Close()
	var recovered protocol.RegisterResponse
	if recoveryResponse.StatusCode != http.StatusOK || json.NewDecoder(recoveryResponse.Body).Decode(&recovered) != nil || recovered.RegistrationID != registrationID {
		t.Fatalf("candidate secret could not recover committed registration: HTTP %d", recoveryResponse.StatusCode)
	}
	if machines, err := server.store.ListMachines(context.Background()); err != nil || len(machines) != 1 || machines[0].MachineID != recovered.MachineID {
		t.Fatalf("recovery created more than one machine: %d, %v", len(machines), err)
	}
}

func TestAgentRunBacksOffAndRecoversAfterPanelReturns(t *testing.T) {
	panelData := t.TempDir()
	if err := os.Chmod(panelData, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordHash, err := security.HashAdminPassword([]byte("d03-retry-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(panelData, passwordHash); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: panelData, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	var offline atomic.Bool
	offlineObserved := make(chan struct{}, 1)
	statusObserved := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			if r.URL.Path == "/api/v1/agent/desired-state" {
				select {
				case offlineObserved <- struct{}{}:
				default:
				}
			}
			writeProtocolError(w, http.StatusServiceUnavailable, protocol.ErrorInvalidMessage, "Panel 暂时不可用")
			return
		}
		tracked := &responseStatusWriter{ResponseWriter: w}
		server.Handler().ServeHTTP(tracked, r)
		if r.URL.Path == "/api/v1/agent/status" && tracked.status == http.StatusOK {
			select {
			case statusObserved <- struct{}{}:
			default:
			}
		}
	})
	tlsServer := httptest.NewTLSServer(handler)
	t.Cleanup(func() { tlsServer.Close(); _ = server.Close() })
	caPath := filepath.Join(t.TempDir(), "retry-panel-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	joinToken, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.CreateJoinToken(context.Background(), security.HashToken(joinToken), "", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "retry-agent")
	if _, err := agent.Pair(context.Background(), agentDir, tlsServer.URL, "Retry Agent", joinToken, caPath, false); err != nil {
		t.Fatal(err)
	}
	client, err := agent.LoadClient(agentDir, caPath)
	if err != nil {
		t.Fatal(err)
	}
	offline.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- client.Run(ctx) }()
	select {
	case <-offlineObserved:
	case <-time.After(3 * time.Second):
		cancel()
		<-runDone
		client.Close()
		t.Fatal("Agent did not observe the temporary Panel outage")
	}
	offline.Store(false)
	select {
	case <-statusObserved:
	case <-time.After(5 * time.Second):
		cancel()
		<-runDone
		client.Close()
		t.Fatal("Agent did not recover and report status after Panel returned")
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		client.Close()
		t.Fatalf("Agent run exited unexpectedly after recovery: %v", err)
	}
	identity := client.Identity()
	client.Close()
	if identity.LastHeartbeat.IsZero() {
		t.Fatal("Agent did not persist heartbeat after recovering")
	}
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatusWriter) Write(contents []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(contents)
}

func loginControlAdmin(t *testing.T, client *http.Client, baseURL, password string) {
	t.Helper()
	token := fetchLoginCSRF(t, client, baseURL)
	response := postForm(t, client, baseURL+"/login", url.Values{"csrf": {token}, "password": {password}}, baseURL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("administrator login returned HTTP %d", response.StatusCode)
	}
}

func fetchMachinesCSRF(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	response := request(t, client, http.MethodGet, baseURL+"/admin/machines", nil, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("machine page returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("machine page did not include CSRF token")
	}
	return string(match[1])
}

func createJoinTokenThroughAdmin(t *testing.T, client *http.Client, baseURL, csrf, target string) string {
	t.Helper()
	response := postForm(t, client, baseURL+"/admin/join-tokens", url.Values{"csrf": {csrf}, "target_machine_id": {target}}, baseURL)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("Join Token page returned HTTP %d: %s", response.StatusCode, body)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`<code>([A-Za-z0-9_-]{43})</code>`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("one-time Join Token was not shown")
	}
	return string(match[1])
}
