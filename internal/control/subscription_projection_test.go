package control

import (
	"encoding/json"
	"strings"
	"testing"

	"panel-agent.local/panel-agent/internal/storage"
)

func TestSubscriptionProjectionOmitsExitMetadata(t *testing.T) {
	access := storage.UserAccess{Nodes: []storage.NodeRecord{{NodeID: "node-1", MachineID: "machine-1", NodeInput: storage.NodeInput{
		Name: "entry", PublicHost: "entry.example.test", PublicPort: 45001, ServerName: "www.cloudflare.com", ExitMachineID: "exit-machine-secret",
	}}}}
	projected := subscriptionNodes(access)
	data, err := json.Marshal(map[string]any{"uri": nodeURI(projected[0], "user-secret"), "mihomo": mihomoPreset(projected, "user-secret")})
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, secret := range []string{"exit_machine_id", "exit-machine-secret", "machine_id", "private_key"} {
		if strings.Contains(output, secret) {
			t.Fatalf("subscription projection leaked %q: %s", secret, output)
		}
	}
}

func TestSubscriptionProjectionUsesTrojanExtensionRenderer(t *testing.T) {
	access := storage.UserAccess{Nodes: []storage.NodeRecord{{NodeID: "node-2", NodeInput: storage.NodeInput{Name: "trojan", PublicHost: "edge.example.test", PublicPort: 443}, ExtensionID: "trojan-tls-v1", ExtensionVersion: "1.0.0", ExtensionKind: "trojan_tls", ExtensionSNI: "edge.example.test"}}}
	projected := subscriptionNodes(access)
	uri := nodeURI(projected[0], "user-secret")
	if !strings.HasPrefix(uri, "trojan://") || !strings.Contains(uri, "sni=edge.example.test") || strings.Contains(uri, "pbk=") {
		t.Fatalf("Trojan extension URI projection = %q", uri)
	}
	proxy := mihomoPreset(projected, "user-secret")["proxies"].([]map[string]any)[0]
	if proxy["type"] != "trojan" || proxy["tls"] != true || proxy["udp"] != false {
		t.Fatalf("Trojan extension Mihomo projection = %#v", proxy)
	}
}
