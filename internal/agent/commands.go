package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func HasPendingPairing(dataDir string) (bool, error) {
	store, err := openIdentityStore(dataDir)
	if err != nil {
		return false, err
	}
	lock, err := storage.AcquireDataLock(store.dataDir, "agent.lock")
	if err != nil {
		return false, err
	}
	defer lock.Close()
	identity, err := store.Load()
	if err != nil {
		return false, err
	}
	return identity.Pending, nil
}

func Pair(ctx context.Context, dataDir, panelURL, displayName, joinToken, caCertPath string, repair bool) (string, error) {
	store, err := openIdentityStore(dataDir)
	if err != nil {
		return "", err
	}
	processLock, err := storage.AcquireDataLock(store.dataDir, "agent.lock")
	if err != nil {
		return "", err
	}
	defer processLock.Close()
	identity, loadErr := store.Load()
	if loadErr == nil && identity.Pending {
		if panelURL != "" && strings.TrimRight(panelURL, "/") != strings.TrimRight(identity.PanelURL, "/") {
			return "", errors.New("pending Agent pairing is bound to a different Panel URL")
		}
		client, err := NewClient(store, identity, caCertPath)
		if err != nil {
			return "", err
		}
		client.processLock = processLock
		defer client.Close()
		if err := client.CompletePairing(ctx); err != nil {
			return "", err
		}
		if err := client.SyncOnce(ctx); err != nil {
			return client.identity.MachineID, err
		}
		return client.identity.MachineID, nil
	}
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return "", loadErr
	}
	if loadErr == nil && !repair {
		return "", errors.New("Agent is already paired; use re-pair to replace its credential")
	}
	if repair && (loadErr != nil || identity.MachineID == "") {
		return "", errors.New("re-pair requires an existing paired Agent identity")
	}
	if !security.IsValidToken(joinToken, security.MachineSecretBytes) {
		return "", errors.New("Join Token is invalid")
	}
	if panelURL == "" {
		if loadErr == nil {
			panelURL = identity.PanelURL
		}
	}
	registrationID, err := security.NewUUID()
	if err != nil {
		return "", err
	}
	candidate, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		return "", err
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "", errors.New("could not determine the machine hostname")
	}
	if loadErr != nil {
		identity = Identity{
			Version:      1,
			PanelURL:     panelURL,
			Hostname:     hostname,
			DisplayName:  strings.TrimSpace(displayName),
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
			CPUCount:     runtime.NumCPU(),
			MemoryBytes:  totalMemoryBytes(),
			AgentVersion: protocol.AgentVersion,
			DesiredState: protocol.EmptyDesiredState(),
		}
	} else {
		if displayName != "" {
			identity.DisplayName = strings.TrimSpace(displayName)
		}
		identity.PanelURL = panelURL
		identity.Hostname = hostname
		identity.OS = runtime.GOOS
		identity.Architecture = runtime.GOARCH
		identity.CPUCount = runtime.NumCPU()
		identity.MemoryBytes = totalMemoryBytes()
		identity.AgentVersion = protocol.AgentVersion
		identity.AuthorityNonce = ""
	}
	identity.RegistrationID = registrationID
	identity.MachineSecret = candidate
	identity.PendingJoinToken = joinToken
	identity.Pending = true
	if repair {
		identity.TargetMachineID = identity.MachineID
	} else {
		identity.MachineID = ""
		identity.TargetMachineID = ""
		identity.AuthorityEpoch = ""
		identity.Revision = 0
		identity.Digest = ""
		identity.DesiredState = protocol.EmptyDesiredState()
	}
	client, err := NewClient(store, identity, caCertPath)
	if err != nil {
		return "", err
	}
	client.processLock = processLock
	defer client.Close()
	if err := store.Save(identity); err != nil {
		return "", err
	}
	client.identity = identity
	if err := client.CompletePairing(ctx); err != nil {
		return "", err
	}
	if err := client.SyncOnce(ctx); err != nil {
		return client.identity.MachineID, err
	}
	return client.identity.MachineID, nil
}

func LoadClient(dataDir, caCertPath string) (*Client, error) {
	store, err := openIdentityStore(dataDir)
	if err != nil {
		return nil, err
	}
	processLock, err := storage.AcquireDataLock(store.dataDir, "agent.lock")
	if err != nil {
		return nil, err
	}
	identity, err := store.Load()
	if err != nil {
		_ = processLock.Close()
		return nil, fmt.Errorf("load Agent identity: %w", err)
	}
	client, err := NewClient(store, identity, caCertPath)
	if err != nil {
		_ = processLock.Close()
		return nil, err
	}
	client.processLock = processLock
	return client, nil
}

func totalMemoryBytes() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	contents, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kilobytes > (1<<60)/1024 {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}
