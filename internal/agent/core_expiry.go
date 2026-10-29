package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

func (m *CoreManager) desiredPath(digest string) string {
	return filepath.Join(m.dir, "desired-"+digest+".json")
}

func (m *CoreManager) readDesired(record *coreConfigRecord) (protocol.DesiredState, error) {
	path := m.desiredPath(record.Digest)
	info, err := os.Lstat(path)
	if err != nil {
		return protocol.DesiredState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return protocol.DesiredState{}, errors.New("invalid private Core source snapshot")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return protocol.DesiredState{}, err
	}
	var desired protocol.DesiredState
	if err := json.Unmarshal(contents, &desired); err != nil {
		return desired, err
	}
	digest, err := protocol.DigestDesiredState(desired)
	if err != nil || digest != record.Digest {
		return desired, errors.New("Core source snapshot digest mismatch")
	}
	return desired, protocol.ValidateDesiredState(desired)
}

func (m *CoreManager) persistDesired(digest string, desired protocol.DesiredState) error {
	if _, err := os.Lstat(m.desiredPath(digest)); err == nil {
		_, err = m.readDesired(&coreConfigRecord{Digest: digest})
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	contents, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	return storage.WritePrivateAtomic(m.desiredPath(digest), contents)
}

func (m *CoreManager) renderDesired(desired protocol.DesiredState) (xray.RenderedConfig, error) {
	if err := protocol.ValidateDesiredState(desired); err != nil {
		return xray.RenderedConfig{}, err
	}
	filtered := desired
	filtered.Nodes = []protocol.DesiredNode{}
	for _, node := range desired.Nodes {
		if node.Extension != nil && node.Extension.Kind == "hysteria2_tls" {
			if !m.hysteriaConfigured {
				return xray.RenderedConfig{}, errors.New("该机器尚未配置官方 Hysteria 内核")
			}
			continue
		}
		filtered.Nodes = append(filtered.Nodes, node)
	}
	if m.developmentLoopback {
		return xray.RenderDevelopmentConfig(filtered)
	}
	return xray.RenderConfig(filtered)
}

// Transport recovery must not undo an already confirmed revocation or secret
// rotation. Older LKG may retain only the (Node, User, UUID) grants still allowed
// by the latest successfully applied policy. It cannot add new grants.
func (m *CoreManager) renderWithPermissionFloor(desired protocol.DesiredState, clamp bool) (xray.RenderedConfig, error) {
	if !clamp || m.state.PermissionDigest == "" {
		return m.renderDesired(desired)
	}
	floor, err := m.readDesired(&coreConfigRecord{Digest: m.state.PermissionDigest})
	if err != nil {
		return xray.RenderedConfig{}, err
	}
	allowed := map[string]map[string]protocol.ProxyCredential{}
	allowedExits := map[string]string{}
	for _, node := range floor.Nodes {
		if node.Enabled {
			allowedExits[node.ID] = node.ExitMachineID
			allowed[node.ID] = map[string]protocol.ProxyCredential{}
			for _, credential := range node.Credentials {
				allowed[node.ID][credential.UserID+"\x00"+credential.UUID] = credential
			}
		}
	}
	filtered := desired
	filtered.Nodes = append([]protocol.DesiredNode{}, desired.Nodes...)
	filtered.PeriodPolicies = floor.PeriodPolicies
	filtered.TrafficReportingEnabled = floor.TrafficReportingEnabled
	filtered.QuotaRestrictions, err = protocol.MergeQuotaRestrictions(floor.QuotaRestrictions, desired.QuotaRestrictions)
	if err != nil {
		return xray.RenderedConfig{}, err
	}
	// Keep complete transport metadata during local expiry checks, while an old
	// LKG may retain only transit secrets and Exit grants still in the floor.
	transit := map[string]bool{}
	filtered.TransitOutbounds = []protocol.TransitOutbound{}
	for _, outbound := range desired.TransitOutbounds {
		for _, current := range floor.TransitOutbounds {
			if outbound.ExitMachineID == current.ExitMachineID && outbound.UUID == current.UUID {
				filtered.TransitOutbounds = append(filtered.TransitOutbounds, outbound)
				transit[outbound.ExitMachineID] = true
				break
			}
		}
	}
	if desired.Exit != nil {
		filtered.Exit = nil
		if floor.Exit != nil && desired.Exit.MachineID == floor.Exit.MachineID {
			exit := *desired.Exit
			exit.Credentials = []protocol.TransitCredential{}
			for _, credential := range desired.Exit.Credentials {
				for _, current := range floor.Exit.Credentials {
					if credential.EntryMachineID == current.EntryMachineID && credential.UUID == current.UUID {
						exit.Credentials = append(exit.Credentials, current)
						break
					}
				}
			}
			filtered.Exit = &exit
		}
	}
	for i := range filtered.Nodes {
		node := &filtered.Nodes[i]
		grants, exists := allowed[node.ID]
		node.Enabled = node.Enabled && exists && node.ExitMachineID == allowedExits[node.ID]
		if node.ExitMachineID != "" && !transit[node.ExitMachineID] {
			node.Enabled = false
		}
		credentials := []protocol.ProxyCredential{}
		for _, credential := range node.Credentials {
			if current, ok := grants[credential.UserID+"\x00"+credential.UUID]; ok {
				credentials = append(credentials, current)
			}
		}
		node.Credentials = credentials
	}
	kept := filtered.TransitOutbounds[:0]
	for _, outbound := range filtered.TransitOutbounds {
		for _, node := range filtered.Nodes {
			if node.Enabled && node.ExitMachineID == outbound.ExitMachineID {
				kept = append(kept, outbound)
				break
			}
		}
	}
	filtered.TransitOutbounds = kept
	return m.renderDesired(filtered)
}

// Rebuild before a restart so an offline recovery cannot resurrect expired
// credentials from Current or LKG. The original immutable source stays private.
func (m *CoreManager) recordAtPresent(record *coreConfigRecord) (*coreConfigRecord, error) {
	if !record.HasSource {
		if m.state.PermissionDigest != "" {
			return nil, errors.New("legacy rollback has no confirmed permission source")
		}
		return record, nil
	} // D04 records predate expiring credentials.
	desired, err := m.readDesired(record)
	if err != nil {
		return nil, err
	}
	rendered, err := m.renderWithPermissionFloor(desired, true)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(rendered.Bytes)
	next := *record
	effectiveRestrictions := desired.QuotaRestrictions
	if m.state.PermissionDigest != "" {
		floor, err := m.readDesired(&coreConfigRecord{Digest: m.state.PermissionDigest})
		if err != nil {
			return nil, err
		}
		next.PeriodPolicies = floor.PeriodPolicies
		effectiveRestrictions, err = protocol.MergeQuotaRestrictions(floor.QuotaRestrictions, desired.QuotaRestrictions)
		if err != nil {
			return nil, err
		}
	}
	next.ConfigHash, next.Mappings, next.Listeners = hex.EncodeToString(hash[:]), rendered.Mappings, rendered.Listeners
	next.RestrictionDigest, err = effectiveRestrictions.Digest()
	if err != nil {
		return nil, err
	}
	if err := storage.WritePrivateAtomic(m.configPath(&next), rendered.Bytes); err != nil {
		return nil, err
	}
	return &next, nil
}

// Only Current supplies the local expiry policy. A received but unacknowledged
// authority snapshot must not gain permissions while Panel is unreachable.
func (m *CoreManager) ReconcileExpiration(ctx context.Context) error {
	m.mu.Lock()
	record := m.state.Current
	if record == nil || !record.HasSource {
		m.mu.Unlock()
		return nil
	}
	desired, err := m.readDesired(record)
	if err == nil && m.state.PermissionDigest != "" {
		_, err = m.readDesired(&coreConfigRecord{Digest: m.state.PermissionDigest})
	}
	if err != nil {
		// Without the confirmed expiry policy we cannot safely keep granting
		// access. Preserve the mapping/gap and stop only this owned Core.
		_, sampleErr := m.captureBoundary(ctx, false)
		if sampleErr != nil {
			_, _ = m.captureBoundary(ctx, true)
		}
		var stopErr error
		if m.state.Process != nil {
			stopErr = stopCoreProcess(ctx, *m.state.Process, m.binary)
		}
		boundaryErr := m.finishBoundary(m.state.Boundary, false)
		next := m.state
		next.LastError = protocol.ErrorCoreStorageFailed
		if stopErr == nil {
			next.Process = nil
		}
		saveErr := m.save(next)
		m.mu.Unlock()
		return errors.Join(err, stopErr, boundaryErr, saveErr)
	}
	m.mu.Unlock()
	return m.apply(ctx, record.AuthorityEpoch, record.Revision, record.Digest, desired, false)
}

func (m *CoreManager) NextExpiry() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.state.Current
	if record == nil || !record.HasSource {
		return time.Time{}
	}
	if m.state.PermissionDigest != "" {
		record = &coreConfigRecord{Digest: m.state.PermissionDigest, HasSource: true}
	}
	desired, err := m.readDesired(record)
	if err != nil {
		return time.Time{}
	}
	var next time.Time
	now := time.Now()
	for _, node := range desired.Nodes {
		if node.Enabled {
			for _, credential := range node.Credentials {
				if credential.ExpiresAt == "" {
					continue
				}
				expiry, err := time.Parse(time.RFC3339, credential.ExpiresAt)
				if err == nil && expiry.After(now) && (next.IsZero() || expiry.Before(next)) {
					next = expiry
				}
			}
		}
	}
	return next
}
