package agent

import (
	"testing"

	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
)

func TestLedgerReadingsKeepsTransitMachineOnly(t *testing.T) {
	readings := ledgerReadings([]boundaryCounter{
		{StatMapping: xray.StatMapping{Kind: "machine_only_transit", Email: "transit-entry-exit"}, Uplink: 11, Downlink: 17},
		{StatMapping: xray.StatMapping{Kind: "user_entry", UserID: "00000000-0000-4000-8000-000000000001", NodeID: "00000000-0000-4000-8000-000000000002", Email: "user-node"}, Uplink: 23, Downlink: 29},
		{StatMapping: xray.StatMapping{Kind: "user_entry", UserID: "development", NodeID: "development-node", Email: "development-node"}, Uplink: 31, Downlink: 37},
	})
	if len(readings) != 2 {
		t.Fatalf("readings = %+v, want one transit and one user reading", readings)
	}
	if readings[0].Stream != (protocol.TrafficStream{Kind: "machine_only_transit"}) || readings[0].Totals != (protocol.Totals{Uplink: quantity.Bytes(11), Downlink: quantity.Bytes(17)}) {
		t.Fatalf("transit reading = %+v", readings[0])
	}
	if readings[1].Stream.Kind != "user_entry" || readings[1].Stream.UserID != "00000000-0000-4000-8000-000000000001" || readings[1].Stream.EntryNodeID != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("user reading attribution = %+v", readings[1])
	}
}
