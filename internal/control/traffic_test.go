package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestTrafficRecoveryAndIngestHTTPSRoutesAuthenticateAndACK(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dataDir, "traffic-control-test-password-hash"); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(server.Handler())
	t.Cleanup(func() {
		tlsServer.Close()
		_ = server.Close()
	})

	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	join, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, authority, err := server.store.RegisterMachine(ctx, storage.RegisterMachineInput{
		RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate),
		Hostname: "traffic-agent", OS: "linux", Architecture: "amd64", CPUCount: 1, MemoryBytes: 128 << 20, AgentVersion: protocol.AgentVersion,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.SaveNode(ctx, machine.MachineID, "", storage.NodeInput{
		Name: "traffic-node", PublicHost: "node.example.test", PublicPort: 443,
		ListenHost: "127.0.0.1", ListenPort: 18443, Enabled: true, ServerName: "www.example.com",
	}, now); err != nil {
		t.Fatal(err)
	}
	desired, err := server.store.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	var policy period.Policy
	for _, candidatePolicy := range desired.Snapshot.PeriodPolicies {
		if candidatePolicy.Scope.Kind == "machine" && candidatePolicy.Scope.MachineID == machine.MachineID {
			policy = candidatePolicy
			break
		}
	}
	if policy.ID == "" {
		t.Fatal("machine traffic policy was not published")
	}
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	trafficPeriod, err := period.At(policy, start)
	if err != nil {
		t.Fatal(err)
	}
	streamID, _ := security.NewUUID()
	segmentID, _ := security.NewUUID()
	ledgerEpoch, _ := security.NewUUID()
	segment := protocol.TrafficSegment{
		Key:    protocol.SegmentKey{MachineID: machine.MachineID, LedgerEpoch: ledgerEpoch, StreamID: streamID, SegmentID: segmentID},
		Stream: protocol.TrafficStream{Kind: "machine_only_transit"}, Periods: []period.Period{trafficPeriod},
		Start: start, Through: start.Add(time.Minute), Totals: protocol.Totals{Uplink: quantity.Bytes(7), Downlink: quantity.Bytes(11)},
	}
	if err := segment.Validate(); err != nil {
		t.Fatal(err)
	}
	recovery := protocol.TrafficRecoveryRequest{Version: protocol.TrafficVersion, RequestID: mustControlUUID(t), AuthorityEpoch: authority.Epoch,
		MachineID: machine.MachineID, LedgerEpoch: ledgerEpoch}
	response := postTrafficJSON(t, tlsServer.Client(), tlsServer.URL+"/api/v1/agent/traffic/recover", candidate, recovery)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("traffic recovery status = %d", response.StatusCode)
	}
	var recoveryResponse protocol.TrafficRecoveryResponse
	if err := json.NewDecoder(response.Body).Decode(&recoveryResponse); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if !recoveryResponse.Resume || recoveryResponse.InstallationID == "" || recoveryResponse.LedgerEpoch != ledgerEpoch {
		t.Fatalf("traffic recovery response = %+v", recoveryResponse)
	}

	request := protocol.TrafficRequest{Version: protocol.TrafficVersion, RequestID: mustControlUUID(t), AuthorityEpoch: authority.Epoch,
		MachineID: machine.MachineID, LedgerEpoch: ledgerEpoch, Segments: []protocol.TrafficSegment{segment}}
	response = postTrafficJSON(t, tlsServer.Client(), tlsServer.URL+"/api/v1/agent/traffic", candidate, request)
	if response.StatusCode != http.StatusOK {
		body := make([]byte, 4096)
		n, _ := response.Body.Read(body)
		response.Body.Close()
		t.Fatalf("traffic ingest status = %d: %s", response.StatusCode, body[:n])
	}
	var ack protocol.TrafficACK
	if err := json.NewDecoder(response.Body).Decode(&ack); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if len(ack.Outcomes) != 1 || ack.Outcomes[0].Status != "cumulative" || ack.Outcomes[0].Committed.Uplink != 7 {
		t.Fatalf("traffic ingest ACK = %+v", ack)
	}

	wrongMachine := request
	wrongMachine.MachineID = mustControlUUID(t)
	wrongMachine.Segments = []protocol.TrafficSegment{segment}
	response = postTrafficJSON(t, tlsServer.Client(), tlsServer.URL+"/api/v1/agent/traffic", candidate, wrongMachine)
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("body-supplied machine identity status = %d", response.StatusCode)
	}
	response.Body.Close()

	plainServer := httptest.NewServer(server.Handler())
	t.Cleanup(plainServer.Close)
	response = postTrafficJSON(t, plainServer.Client(), plainServer.URL+"/api/v1/agent/traffic/recover", candidate, recovery)
	if response.StatusCode != http.StatusUpgradeRequired {
		response.Body.Close()
		t.Fatalf("plaintext traffic endpoint status = %d", response.StatusCode)
	}
	response.Body.Close()
}

func mustControlUUID(t *testing.T) string {
	t.Helper()
	id, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func postTrafficJSON(t *testing.T, client *http.Client, endpoint, bearer string, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
