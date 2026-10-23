package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

const identityFileName = "agent.identity.json"

type Identity struct {
	Version          int                   `json:"version"`
	PanelURL         string                `json:"panel_url"`
	MachineID        string                `json:"machine_id,omitempty"`
	RegistrationID   string                `json:"registration_id"`
	MachineSecret    string                `json:"machine_secret"`
	PendingJoinToken string                `json:"pending_join_token,omitempty"`
	TargetMachineID  string                `json:"target_machine_id,omitempty"`
	Pending          bool                  `json:"pending"`
	Hostname         string                `json:"hostname"`
	DisplayName      string                `json:"display_name,omitempty"`
	OS               string                `json:"os"`
	Architecture     string                `json:"architecture"`
	CPUCount         int                   `json:"cpu_count"`
	MemoryBytes      uint64                `json:"memory_bytes"`
	AgentVersion     string                `json:"agent_version"`
	AuthorityEpoch   string                `json:"authority_epoch,omitempty"`
	Revision         uint64                `json:"revision,omitempty"`
	Digest           string                `json:"digest,omitempty"`
	AuthorityNonce   string                `json:"authority_nonce,omitempty"`
	DesiredState     protocol.DesiredState `json:"desired_state"`
	LastHeartbeat    time.Time             `json:"last_heartbeat,omitempty"`
}

type identityStore struct {
	dataDir string
	path    string
}

func openIdentityStore(dataDir string) (*identityStore, error) {
	absolute, err := storage.PrepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	return &identityStore{dataDir: absolute, path: filepath.Join(absolute, identityFileName)}, nil
}

func (s *identityStore) Load() (Identity, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		return Identity{}, err
	}
	if !info.Mode().IsRegular() {
		return Identity{}, errors.New("Agent identity must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return Identity{}, errors.New("Agent identity permissions must be 0600")
	}
	contents, err := os.ReadFile(s.path)
	if err != nil {
		return Identity{}, fmt.Errorf("read Agent identity: %w", err)
	}
	var identity Identity
	if err := json.Unmarshal(contents, &identity); err != nil {
		return Identity{}, fmt.Errorf("decode Agent identity: %w", err)
	}
	if identity.Version != 1 || identity.RegistrationID == "" || !validSecret(identity.MachineSecret) {
		return Identity{}, errors.New("Agent identity is incomplete")
	}
	return identity, nil
}

func (s *identityStore) Save(identity Identity) error {
	contents, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Agent identity: %w", err)
	}
	return storage.WritePrivateAtomic(s.path, contents)
}
