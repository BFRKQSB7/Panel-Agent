package agent

import (
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/core/hysteria"
	"panel-agent.local/panel-agent/internal/protocol"
)

func TestHysteriaAuthorizationAndAttribution(t *testing.T) {
	const node = "00000000-0000-4000-8000-000000000001"
	const user = "00000000-0000-4000-8000-000000000002"
	const secret = "00000000-0000-4000-8000-000000000003"
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	n := protocol.DesiredNode{ID: node, Enabled: true, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: secret, Email: "user-node", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}}}
	accounts, err := hysteriaAccounts(n, protocol.QuotaRestrictions{}, now)
	if err != nil || len(accounts) != 1 || accounts[0].ID != "user-node" {
		t.Fatalf("auth mapping: %v", err)
	}
	for _, r := range []protocol.QuotaRestrictions{
		{MachineStop: true},
		{Items: []protocol.QuotaRestriction{{UserID: user, Reasons: []string{protocol.RestrictionUserQuotaExhausted}}}},
		{Items: []protocol.QuotaRestriction{{UserID: user, NodeID: node, Reasons: []string{protocol.RestrictionNodeQuotaExhausted}}}},
	} {
		accounts, err := hysteriaAccounts(n, r, now)
		if err != nil || len(accounts) != 0 {
			t.Fatalf("restricted account admitted: %v", err)
		}
	}
	if a, err := hysteriaAccounts(n, protocol.QuotaRestrictions{}, now.Add(time.Hour)); err != nil || len(a) != 0 {
		t.Fatal("expired account admitted")
	}
	readings, err := hysteriaReadings(node, n.Credentials, map[string]hysteria.Counter{"user-node": {TX: 9007199254740993, RX: 42}})
	if err != nil || len(readings) != 1 || readings[0].Stream.UserID != user || readings[0].Stream.EntryNodeID != node || readings[0].Totals.Uplink != 9007199254740993 || readings[0].Totals.Downlink != 42 {
		t.Fatalf("incorrect attribution: %v", err)
	}
	if _, err := hysteriaReadings(node, n.Credentials, map[string]hysteria.Counter{"unknown": {TX: 1}}); err == nil {
		t.Fatal("unmapped traffic discarded")
	}
	if _, err := hysteriaReadings(node, n.Credentials, map[string]hysteria.Counter{"user-node": {RX: -1}}); err == nil {
		t.Fatal("negative traffic accepted")
	}
}
