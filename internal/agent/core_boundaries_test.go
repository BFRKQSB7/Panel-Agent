package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/security"
)

func TestLostCandidateRetainsIdentityAndExplicitGap(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "boundaries"), 0o700); err != nil {
		t.Fatal(err)
	}
	instance, _ := security.NewUUID()
	epoch, _ := security.NewUUID()
	m := &CoreManager{dir: dir, state: coreJournal{Version: 1, Phase: "checking", Generation: 8, InstanceID: instance, Candidate: &coreConfigRecord{AuthorityEpoch: epoch, Revision: 4, Mappings: []xray.StatMapping{{UserID: "new-user", NodeID: "new-node", Email: "new-label"}}}}}
	if err := m.captureLostCandidate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "boundaries"))
	if err != nil || len(entries) != 1 {
		t.Fatal("candidate gap missing", err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "boundaries", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var boundary coreBoundary
	if err := json.Unmarshal(contents, &boundary); err != nil {
		t.Fatal(err)
	}
	if boundary.Generation != 8 || boundary.InstanceID != instance || len(boundary.Config.Mappings) != 1 || boundary.Config.Mappings[0].UserID != "new-user" || len(boundary.Counters) != 0 || !boundary.UnobservedTail || boundary.ObservationGap == "" {
		t.Fatal("lost candidate was assigned zeros or lost its identity")
	}
}
