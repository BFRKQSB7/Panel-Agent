package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestCoreStatusGenerationAcrossBackendChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openIdentityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{store: store}
	if err := os.Mkdir(filepath.Join(store.dataDir, "core"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dataDir, "core", "state.json"), []byte(`{"core_generation":90}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, _ := security.NewUUID()
	b, _ := security.NewUUID()
	report := func(kind string, gen uint64, id string) uint64 {
		t.Helper()
		r := protocol.StatusRequest{CoreGeneration: gen, CoreInstanceID: id}
		if err := c.mapCoreStatus(kind, &r); err != nil {
			t.Fatal(err)
		}
		return r.CoreGeneration
	}
	first := report("hysteria", 2, a)
	if first <= 90 {
		t.Fatal("old Xray generation not fenced")
	}
	if got := report("hysteria", 2, a); got != first {
		t.Fatal("unchanged report changed generation")
	}
	// Reload the persisted identity as after an interrupted status HTTP request.
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	c = &Client{store: store, identity: identity}
	if got := report("hysteria", 2, a); got != first {
		t.Fatal("retry after restart changed fence")
	}
	second := report("xray", 1, b)
	if second <= first {
		t.Fatal("backend switch regressed generation")
	}
	third := report("hysteria", 3, a)
	if third <= second {
		t.Fatal("return switch regressed generation")
	}
}

func TestCoreStatusRejectsCorruptMigrationJournal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openIdentityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dataDir, "hysteria-state.json"), []byte(`{"Generation":9223372036854775807}`), 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := security.NewUUID()
	c := &Client{store: store}
	r := protocol.StatusRequest{CoreGeneration: 1, CoreInstanceID: id}
	if err := c.mapCoreStatus("xray", &r); err == nil {
		t.Fatal("exhausted migration floor accepted")
	}
	if c.identity.StatusGeneration != 0 {
		t.Fatal("failed migration mutated status identity")
	}
}
