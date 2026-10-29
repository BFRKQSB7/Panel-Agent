package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestOfficialOmittedXrayStopsOwnedProcess(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_XRAY")
	if binary == "" {
		t.Skip("PANEL_AGENT_TEST_XRAY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	machine, _ := security.NewUUID()
	authority, _ := security.NewUUID()
	book, err := ledger.Open(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	m, err := NewCoreManager(dir, binary, false, true)
	if err != nil {
		t.Fatal(err)
	}
	m.ledger = book
	desired := protocol.EmptyDesiredState()
	digest, err := protocol.DigestDesiredState(desired)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Apply(ctx, authority, 1, digest, desired); err != nil {
		t.Fatal(err)
	}
	if m.state.Process == nil {
		t.Fatal("missing original process")
	}
	old := *m.state.Process
	defer stopCoreProcess(context.Background(), old, binary)
	if err = retireOmittedXray(ctx, dir, book); err != nil {
		t.Fatal(err)
	}
	if _, err = inspectCoreProcess(old.PID, binary); err == nil {
		t.Fatal("omitted Xray is still running")
	}
	restored, err := NewCoreManager(dir, binary, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if restored.state.Process != nil || restored.state.Current != nil || restored.state.LKG != nil {
		t.Fatal("retired Xray can restore old permissions")
	}
}
