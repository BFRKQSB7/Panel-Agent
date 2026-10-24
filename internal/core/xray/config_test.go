package xray

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestRenderMergesNodesWithDistinctStatsAndNoImplicitCredentials(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uuid, _ := security.NewUUID()
	state := protocol.EmptyDesiredState()
	for i := 0; i < 3; i++ {
		id, _ := security.NewUUID()
		node := protocol.DesiredNode{ID: id, Name: "测试入口", PublicHost: "node.example.test", PublicPort: 45001 + i, ListenHost: "0.0.0.0", ListenPort: 18443 + i, Enabled: true,
			Reality: protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{}}
		if i < 2 {
			node.Credentials = append(node.Credentials, protocol.ProxyCredential{UserID: "development", UUID: uuid, Email: "development-" + id, Development: true})
		}
		state.Nodes = append(state.Nodes, node)
	}
	rendered, err := RenderConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Inbounds []struct {
			Settings struct{ Clients []struct{ ID, Email string } }
		}
	}
	if err := json.Unmarshal(rendered.Bytes, &config); err != nil {
		t.Fatal(err)
	}
	clients, empty := 0, 0
	for _, inbound := range config.Inbounds {
		clients += len(inbound.Settings.Clients)
		if len(inbound.Settings.Clients) == 0 {
			empty++
		}
	}
	if len(config.Inbounds) != 3 || clients != 2 || empty != 1 || len(rendered.Mappings) != 2 || rendered.Mappings[0].Email == rendered.Mappings[1].Email {
		t.Fatal("merged configuration lost Node or credential/statistics isolation")
	}
	if bytes.Contains(rendered.Bytes, []byte("finalRules")) {
		t.Fatal("production config unexpectedly allows benchmark loopback targets")
	}
	development, err := RenderDevelopmentConfig(state)
	if err != nil || !bytes.Contains(development.Bytes, []byte("18081")) {
		t.Fatal("explicit development mode omitted isolated UDP target")
	}
	var devConfig struct {
		Routing struct {
			Rules []struct {
				InboundTag []string `json:"inboundTag"`
			}
		}
	}
	if err := json.Unmarshal(development.Bytes, &devConfig); err != nil || len(devConfig.Routing.Rules) != 1 || len(devConfig.Routing.Rules[0].InboundTag) != 2 {
		t.Fatal("development target exception includes a normal Node")
	}
	state.Nodes[0], state.Nodes[2] = state.Nodes[2], state.Nodes[0]
	state.Nodes[0].Name, state.Nodes[0].PublicPort = "改名", 45101
	again, err := RenderConfig(state)
	if err != nil || !bytes.Equal(rendered.Bytes, again.Bytes) {
		t.Fatal("metadata/order-only change unexpectedly restarts Core")
	}
	state.Nodes[1].Credentials = append(state.Nodes[1].Credentials, state.Nodes[1].Credentials[0])
	if _, err := RenderConfig(state); err == nil {
		t.Fatal("duplicate credential within a Node was accepted")
	}
}

func TestRenderAppliesGlobalNodeAndMachineQuotaRestrictions(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userOne, _ := security.NewUUID()
	userTwo, _ := security.NewUUID()
	nodeOne, _ := security.NewUUID()
	nodeTwo, _ := security.NewUUID()
	uuidOne, _ := security.NewUUID()
	uuidTwo, _ := security.NewUUID()
	newNode := func(id string, port int) protocol.DesiredNode {
		return protocol.DesiredNode{ID: id, Name: id, PublicHost: "node.example.test", PublicPort: port, ListenHost: "127.0.0.1", ListenPort: port,
			Enabled: true, Reality: protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}, Credentials: []protocol.ProxyCredential{}}
	}
	first, second := newNode(nodeOne, 18443), newNode(nodeTwo, 18444)
	first.Credentials = []protocol.ProxyCredential{{UserID: userOne, UUID: uuidOne, Email: "one"}, {UserID: userTwo, UUID: uuidTwo, Email: "two-one"}}
	second.Credentials = []protocol.ProxyCredential{{UserID: userOne, UUID: uuidOne, Email: "one-two"}, {UserID: userTwo, UUID: uuidTwo, Email: "two-two"}}
	state := protocol.DesiredState{Nodes: []protocol.DesiredNode{first, second}, QuotaRestrictions: protocol.QuotaRestrictions{Items: []protocol.QuotaRestriction{{UserID: userOne, Reasons: []string{protocol.RestrictionUserQuotaExhausted}}, {UserID: userTwo, NodeID: nodeTwo, Reasons: []string{protocol.RestrictionNodeQuotaExhausted}}}}}
	rendered, err := RenderConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Inbounds []struct {
			Tag      string
			Settings struct {
				Clients []struct {
					ID string
				}
			}
		}
	}
	if err := json.Unmarshal(rendered.Bytes, &config); err != nil {
		t.Fatal(err)
	}
	clientsByTag := map[string][]string{}
	for _, inbound := range config.Inbounds {
		for _, client := range inbound.Settings.Clients {
			clientsByTag[inbound.Tag] = append(clientsByTag[inbound.Tag], client.ID)
		}
	}
	if len(config.Inbounds) != 2 || len(clientsByTag["node-"+nodeOne]) != 1 || clientsByTag["node-"+nodeOne][0] != uuidTwo || len(clientsByTag["node-"+nodeTwo]) != 0 {
		t.Fatalf("quota restrictions were not rendered per dimension: %+v", config.Inbounds)
	}
	if len(rendered.Mappings) != 1 || rendered.Mappings[0].UserID != userTwo || rendered.Mappings[0].NodeID != nodeOne {
		t.Fatalf("quota restriction mappings = %+v", rendered.Mappings)
	}
	state.QuotaRestrictions = protocol.QuotaRestrictions{MachineStop: true}
	stopped, err := RenderConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stopped.Bytes, []byte("node-"+nodeOne)) || len(stopped.Mappings) != 0 {
		t.Fatal("machine hard stop left a proxy listener or credential")
	}
}
