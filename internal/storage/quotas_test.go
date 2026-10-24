package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

type quotaFixture struct {
	store   *Store
	now     time.Time
	machine MachineRecord
	node    NodeRecord
	user    UserRecord
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	store := openMachineTestStore(t)
	ctx := context.Background()
	if _, err := store.InitializeAdmin(ctx, "quota-test-password-hash"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	join, candidate, registration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.SaveNode(ctx, machine.MachineID, "", NodeInput{Name: "quota-node", PublicHost: "node.example.test", PublicPort: 45001, ListenHost: "127.0.0.1", ListenPort: 18443, Enabled: true, ServerName: "www.cloudflare.com"}, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeGroup, err := store.SaveGroup(ctx, "node", "", GroupInput{Name: "quota-nodes", NodeIDs: []string{node.NodeID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	userGroup, err := store.SaveGroup(ctx, "user", "", GroupInput{Name: "quota-users", NodeGroupIDs: []string{nodeGroup.GroupID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.SaveUser(ctx, "", UserInput{Name: "quota-user", UserGroupID: userGroup.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	return &quotaFixture{store: store, now: now, machine: machine, node: node, user: user}
}

func (f *quotaFixture) state(t *testing.T) protocol.DesiredState {
	t.Helper()
	authority, err := f.store.GetMachineDesiredState(context.Background(), f.machine.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	return authority.Snapshot
}

func (f *quotaFixture) period(t *testing.T, scope period.Scope) period.Period {
	t.Helper()
	state := f.state(t)
	for _, policy := range state.PeriodPolicies {
		if policy.Scope == scope {
			current, err := period.At(policy, f.now)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}
	}
	t.Fatalf("missing policy for %v", scope)
	return period.Period{}
}

func (f *quotaFixture) addUsage(t *testing.T, scope period.Scope, amount int64) {
	t.Helper()
	current := f.period(t, scope)
	epoch := mustUUID(t)
	if _, err := f.store.db.Exec(`INSERT INTO traffic_usage(machine_id,ledger_epoch,scope_key,period_id,uplink,downlink,updated_at) VALUES(?,?,?,?,?,?,?)`,
		f.machine.MachineID, epoch, scope.Key(), current.ID, amount, 0, f.now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaDimensionsAndMachineBudgetPublishRestrictions(t *testing.T) {
	f := newQuotaFixture(t)
	defer f.store.Close()
	ctx := context.Background()
	global := quantity.Bytes(1000)
	if _, err := f.store.SaveUser(ctx, f.user.UserID, UserInput{Name: f.user.Name, UserGroupID: f.user.UserGroupID, Enabled: true, QuotaBytes: &global}, f.now); err != nil {
		t.Fatal(err)
	}
	defaultNode := quantity.Bytes(100)
	input := f.node.NodeInput
	input.QuotaBytes = &defaultNode
	if _, err := f.store.SaveNode(ctx, f.machine.MachineID, f.node.NodeID, input, f.now); err != nil {
		t.Fatal(err)
	}
	f.addUsage(t, period.Scope{Kind: "user", UserID: f.user.UserID}, 150)
	f.addUsage(t, period.Scope{Kind: "user_node", UserID: f.user.UserID, NodeID: f.node.NodeID}, 150)
	override := quantity.Bytes(200)
	if err := f.store.SaveUserNodeQuota(ctx, f.user.UserID, f.node.NodeID, &override, f.now); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.QuotaRestrictions.Items) != 0 {
		t.Fatalf("node override unexpectedly exhausted global quota: %+v", state.QuotaRestrictions)
	}
	override = 100
	if err := f.store.SaveUserNodeQuota(ctx, f.user.UserID, f.node.NodeID, &override, f.now); err != nil {
		t.Fatal(err)
	}
	state = f.state(t)
	if len(state.QuotaRestrictions.Items) != 1 || state.QuotaRestrictions.Items[0].UserID != f.user.UserID || state.QuotaRestrictions.Items[0].NodeID != f.node.NodeID || state.QuotaRestrictions.Items[0].Reasons[0] != protocol.RestrictionNodeQuotaExhausted {
		t.Fatalf("node quota restriction = %+v", state.QuotaRestrictions)
	}
	summary, err := f.store.GetUserQuotaSummary(ctx, f.user.UserID, f.now)
	if err != nil || len(summary.Nodes) != 1 || summary.Nodes[0].Usage.UsedBytes != 150 || summary.Nodes[0].StatusText != "已耗尽" {
		t.Fatalf("user quota summary = %+v, %v", summary, err)
	}

	machineUsage := period.Scope{Kind: "machine", MachineID: f.machine.MachineID}
	f.addUsage(t, machineUsage, 75)
	warning, budget := quantity.Bytes(50), quantity.Bytes(100)
	if err := f.store.SaveMachineBudget(ctx, MachineBudget{MachineID: f.machine.MachineID, BudgetBytes: &budget, WarningBytes: &warning}, f.now); err != nil {
		t.Fatal(err)
	}
	machineSummary, err := f.store.GetMachineQuotaSummary(ctx, f.machine.MachineID, f.now)
	if err != nil || !machineSummary.SoftWarning || machineSummary.Usage.UsedBytes != 75 {
		t.Fatalf("machine soft budget = %+v, %v", machineSummary, err)
	}
	if f.state(t).QuotaRestrictions.MachineStop {
		t.Fatal("soft machine budget stopped the machine")
	}
	hardBudget := quantity.Bytes(75)
	if err := f.store.SaveMachineBudget(ctx, MachineBudget{MachineID: f.machine.MachineID, BudgetBytes: &hardBudget, WarningBytes: &warning, HardStop: true}, f.now); err != nil {
		t.Fatal(err)
	}
	if !f.state(t).QuotaRestrictions.MachineStop {
		t.Fatal("hard machine budget did not stop the machine")
	}
}

func TestQuotaRecoveryFenceRetainsRestrictionUntilPendingClears(t *testing.T) {
	f := newQuotaFixture(t)
	defer f.store.Close()
	ctx := context.Background()
	zero := quantity.Bytes(0)
	if _, err := f.store.SaveUser(ctx, f.user.UserID, UserInput{Name: f.user.Name, UserGroupID: f.user.UserGroupID, Enabled: true, QuotaBytes: &zero}, f.now); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.QuotaRestrictions.Items) != 1 || state.QuotaRestrictions.Items[0].NodeID != "" {
		t.Fatalf("initial global restriction = %+v", state.QuotaRestrictions)
	}
	install, err := readInstallationForTest(f.store)
	if err != nil {
		t.Fatal(err)
	}
	epoch := mustUUID(t)
	if _, err := f.store.db.Exec(`INSERT INTO traffic_ledgers(machine_id,ledger_epoch,installation_id,allocated_close_seq,closed_through,reconciliation_pending,updated_at) VALUES(?,?,?,?,?,?,?)`, f.machine.MachineID, epoch, install, 0, 0, 1, f.now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SaveUser(ctx, f.user.UserID, UserInput{Name: f.user.Name, UserGroupID: f.user.UserGroupID, Enabled: true}, f.now); err != nil {
		t.Fatal(err)
	}
	if len(f.state(t).QuotaRestrictions.Items) != 1 {
		t.Fatal("pending reconciliation removed the old restriction")
	}
	summary, err := f.store.GetMachineQuotaSummary(ctx, f.machine.MachineID, f.now)
	if err != nil || !summary.Usage.PendingReconcile || summary.Usage.Accurate {
		t.Fatalf("pending machine summary = %+v, %v", summary, err)
	}
	if _, err := f.store.db.Exec("UPDATE traffic_ledgers SET reconciliation_pending=0 WHERE machine_id=?", f.machine.MachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SaveUser(ctx, f.user.UserID, UserInput{Name: f.user.Name, UserGroupID: f.user.UserGroupID, Enabled: true}, f.now); err != nil {
		t.Fatal(err)
	}
	if len(f.state(t).QuotaRestrictions.Items) != 0 {
		t.Fatal("cleared reconciliation did not remove the now-unlimited restriction")
	}
}

func TestQuotaBytesPersistAsExactJSONAndZeroIsNotUnlimited(t *testing.T) {
	f := newQuotaFixture(t)
	defer f.store.Close()
	ctx := context.Background()
	zero := quantity.Bytes(0)
	if _, err := f.store.SaveNode(ctx, f.machine.MachineID, f.node.NodeID, NodeInput{Name: f.node.Name, PublicHost: f.node.PublicHost, PublicPort: f.node.PublicPort, ListenHost: f.node.ListenHost, ListenPort: f.node.ListenPort, Enabled: true, ServerName: f.node.ServerName, Target: f.node.Target, QuotaBytes: &zero}, f.now); err != nil {
		t.Fatal(err)
	}
	nodes, err := f.store.ListNodes(ctx, f.machine.MachineID)
	if err != nil || len(nodes) != 1 || nodes[0].QuotaBytes == nil || *nodes[0].QuotaBytes != 0 {
		t.Fatalf("zero node quota = %+v, %v", nodes, err)
	}
	encoded, err := json.Marshal(nodes[0])
	if err != nil || !bytes.Contains(encoded, []byte(`"quota_bytes":"0"`)) {
		t.Fatalf("node quota JSON = %s, %v", encoded, err)
	}
	if err := f.store.SaveUserNodeQuota(ctx, f.user.UserID, f.node.NodeID, nil, f.now); err != nil {
		t.Fatal(err)
	}
	record, err := f.store.GetUserNodeQuota(ctx, f.user.UserID, f.node.NodeID)
	if err != nil || !record.Exists || record.QuotaBytes != nil {
		t.Fatalf("explicit unlimited override = %+v, %v", record, err)
	}
	if !errors.Is(f.store.SaveMachineBudget(ctx, MachineBudget{MachineID: f.machine.MachineID, BudgetBytes: func() *quantity.Bytes { v := quantity.Bytes(10); return &v }(), WarningBytes: func() *quantity.Bytes { v := quantity.Bytes(11); return &v }()}, f.now), ErrInvalidQuota) {
		t.Fatal("warning above budget was accepted")
	}
}
