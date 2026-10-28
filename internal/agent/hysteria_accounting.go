package agent

import (
	"errors"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

// hysteriaAccounts projects one applied Node's authorization, including all
// three restriction scopes. The caller retains the original credentials as the
// accounting map until the old process has stopped and its source is finalized.
func hysteriaAccounts(node protocol.DesiredNode, restrictions protocol.QuotaRestrictions, at time.Time) ([]hysteria.Account, error) {
	if !security.IsUUIDv4(node.ID) || at.IsZero() {
		return nil, errors.New("hysteria: invalid authorization context")
	}
	if err := restrictions.Validate(); err != nil {
		return nil, err
	}
	accounts := []hysteria.Account{}
	seenIDs, seenSecrets := map[string]bool{}, map[string]bool{}
	for _, c := range node.Credentials {
		if !security.IsUUIDv4(c.UUID) || c.UserID != "development" && !security.IsUUIDv4(c.UserID) || c.Email == "" || len(c.Email) > 128 || seenIDs[c.Email] || seenSecrets[c.UUID] {
			return nil, errors.New("hysteria: invalid credential mapping")
		}
		seenIDs[c.Email], seenSecrets[c.UUID] = true, true
		var expiry time.Time
		if c.ExpiresAt != "" {
			var err error
			expiry, err = time.Parse(time.RFC3339, c.ExpiresAt)
			if err != nil {
				return nil, errors.New("hysteria: invalid credential expiry")
			}
		}
		blocked := !node.Enabled || restrictions.MachineStop || !expiry.IsZero() && !at.Before(expiry)
		for _, r := range restrictions.Items {
			if r.UserID == c.UserID && (r.NodeID == "" || r.NodeID == node.ID) {
				blocked = true
				break
			}
		}
		if !blocked {
			accounts = append(accounts, hysteria.Account{ID: c.Email, Secret: c.UUID, ExpiresAt: expiry})
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts, nil
}

// hysteriaReadings preserves exact counter direction and original attribution.
// Unknown identities cannot silently turn into free usage. Counter continuity
// and missing previously observed identities are checked by the source owner.
func hysteriaReadings(nodeID string, credentials []protocol.ProxyCredential, counters map[string]hysteria.Counter) ([]ledger.Reading, error) {
	if !security.IsUUIDv4(nodeID) {
		return nil, errors.New("hysteria: invalid accounting node")
	}
	mappings := map[string]protocol.ProxyCredential{}
	for _, c := range credentials {
		if c.Email == "" || len(c.Email) > 128 || c.UserID != "development" && !security.IsUUIDv4(c.UserID) {
			return nil, errors.New("hysteria: invalid accounting identity")
		}
		if _, exists := mappings[c.Email]; exists {
			return nil, errors.New("hysteria: duplicate accounting identity")
		}
		mappings[c.Email] = c
	}
	readings := make([]ledger.Reading, 0, len(counters))
	for id, counter := range counters {
		c, exists := mappings[id]
		if !exists || counter.TX < 0 || counter.RX < 0 {
			return nil, errors.New("hysteria: unmapped or invalid traffic observation")
		}
		if c.UserID == "development" {
			continue
		}
		readings = append(readings, ledger.Reading{Label: id, Stream: protocol.TrafficStream{Kind: "user_entry", UserID: c.UserID, EntryNodeID: nodeID}, Totals: protocol.Totals{Uplink: quantity.Bytes(counter.TX), Downlink: quantity.Bytes(counter.RX)}})
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].Label < readings[j].Label })
	return readings, nil
}
