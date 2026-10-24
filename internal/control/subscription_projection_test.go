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
