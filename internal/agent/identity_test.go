package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestIdentityPersistsSecretAtomicallyAndUsesOneAgentProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-data")
	store, err := openIdentityStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registrationID, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		Version: 1, PanelURL: "https://panel.example", MachineID: "550e8400-e29b-41d4-a716-446655440000",
		RegistrationID: registrationID, MachineSecret: secret, Hostname: "agent-host", OS: "linux", Architecture: "amd64",
		AgentVersion: protocol.AgentVersion, DesiredState: protocol.EmptyDesiredState(),
	}
	if err := store.Save(identity); err != nil {
		t.Fatal(err)
	}
	identity.DisplayName = "updated"
	if err := store.Save(identity); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MachineSecret != secret || loaded.DisplayName != "updated" {
		t.Fatal("Agent identity did not survive atomic replacement")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("identity permissions = %o, want 600", info.Mode().Perm())
		}
	}

	firstLock, err := storage.AcquireDataLock(store.dataDir, "agent.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AcquireDataLock(store.dataDir, "agent.lock"); err == nil {
		t.Fatal("second Agent process acquired the identity lock")
	}
	if err := firstLock.Close(); err != nil {
		t.Fatal(err)
	}
	secondLock, err := storage.AcquireDataLock(store.dataDir, "agent.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := secondLock.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}
