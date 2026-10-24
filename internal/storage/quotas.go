package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

var ErrInvalidQuota = errors.New("额度字段无效")

type UserNodeQuotaRecord struct {
	UserID     string
	NodeID     string
	QuotaBytes *quantity.Bytes
	Exists     bool
}

type MachineBudget struct {
	MachineID    string
	BudgetBytes  *quantity.Bytes `json:"budget_bytes"`
	WarningBytes *quantity.Bytes `json:"warning_bytes"`
	HardStop     bool            `json:"hard_stop"`
}

type QuotaUsage struct {
	Scope            string          `json:"scope"`
	UserID           string          `json:"user_id,omitempty"`
	NodeID           string          `json:"node_id,omitempty"`
	MachineID        string          `json:"machine_id,omitempty"`
	PeriodID         string          `json:"period_id,omitempty"`
	PeriodStart      string          `json:"period_start,omitempty"`
	PeriodEnd        string          `json:"period_end,omitempty"`
	UsedBytes        quantity.Bytes  `json:"used_bytes"`
	LimitBytes       *quantity.Bytes `json:"limit_bytes"`
	RemainingBytes   *quantity.Bytes `json:"remaining_bytes"`
	LastUpdated      string          `json:"last_updated,omitempty"`
	PendingReconcile bool            `json:"pending_reconcile"`
	Accurate         bool            `json:"accurate"`
	Status           []string        `json:"status,omitempty"`
}

type UserNodeQuotaView struct {
	NodeID         string          `json:"node_id"`
	NodeName       string          `json:"node_name"`
	MachineID      string          `json:"machine_id"`
	DefaultQuota   *quantity.Bytes `json:"default_quota"`
	HasOverride    bool            `json:"has_override"`
	OverrideQuota  *quantity.Bytes `json:"override_quota"`
	EffectiveQuota *quantity.Bytes `json:"effective_quota"`
	Usage          QuotaUsage      `json:"usage"`
	StatusText     string          `json:"status_text"`
}

type UserQuotaSummary struct {
	UserID  string              `json:"user_id"`
	Global  QuotaUsage          `json:"global"`
	Nodes   []UserNodeQuotaView `json:"nodes"`
	Reasons []string            `json:"reasons"`
}

type MachineQuotaSummary struct {
	MachineID            string        `json:"machine_id"`
	Budget               MachineBudget `json:"budget"`
	Usage                QuotaUsage    `json:"usage"`
	SoftWarning          bool          `json:"soft_warning"`
	RestrictionDigest    string        `json:"restriction_digest,omitempty"`
	RestrictionAppliedAt string        `json:"restriction_applied_at,omitempty"`
}

type quotaQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateQuotaValue(value *quantity.Bytes) error {
	if value != nil && *value < 0 {
		return ErrInvalidQuota
	}
	return nil
}

func copyQuota(value *quantity.Bytes) *quantity.Bytes {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func quotaStatus(limit *quantity.Bytes, used quantity.Bytes) (*quantity.Bytes, []string) {
	if limit == nil {
		return nil, nil
	}
	if used >= *limit {
		zero := quantity.Bytes(0)
		return &zero, []string{"exhausted"}
	}
	remaining := *limit - used
	return &remaining, []string{"available"}
}

func readTrafficUsageTotalTx(ctx context.Context, tx *sql.Tx, scopeKey, periodID string) (quantity.Bytes, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uplink,downlink,updated_at FROM traffic_usage
		WHERE scope_key=? AND period_id=? ORDER BY updated_at`, scopeKey, periodID)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var up, down quantity.Bytes
	var last string
	for rows.Next() {
		var rowUp, rowDown int64
		var updated string
		if err := rows.Scan(&rowUp, &rowDown, &updated); err != nil {
			return 0, "", err
		}
		var addErr error
		up, addErr = quantity.Add(up, quantity.Bytes(rowUp))
		if addErr != nil {
			return 0, "", addErr
		}
		down, addErr = quantity.Add(down, quantity.Bytes(rowDown))
		if addErr != nil {
			return 0, "", addErr
		}
		if updated > last {
			last = updated
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	total, err := quantity.Add(up, down)
	return total, last, err
}

func currentPolicyTx(ctx context.Context, tx *sql.Tx, scope period.Scope, now time.Time) (period.Policy, bool, error) {
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT policy_json FROM period_policies
		WHERE scope_key=? AND effective_at<=? ORDER BY effective_at DESC,policy_id DESC LIMIT 1`,
		scope.Key(), now.UTC().Format(time.RFC3339Nano)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return period.Policy{}, false, nil
	}
	if err != nil {
		return period.Policy{}, false, err
	}
	var policy period.Policy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil || policy.Validate() != nil {
		return period.Policy{}, false, ErrInvalidQuota
	}
	return policy, true, nil
}

func quotaUsageTx(ctx context.Context, tx *sql.Tx, scope period.Scope, limit *quantity.Bytes, pending bool, now time.Time) (QuotaUsage, error) {
	view := QuotaUsage{Scope: scope.Kind, UserID: scope.UserID, NodeID: scope.NodeID, MachineID: scope.MachineID, LimitBytes: copyQuota(limit), PendingReconcile: pending, Accurate: !pending}
	policy, found, err := currentPolicyTx(ctx, tx, scope, now)
	if err != nil {
		return view, err
	}
	if !found {
		view.Accurate = false
		view.Status = []string{"unknown_policy"}
		if pending {
			view.Status = append(view.Status, "pending_reconcile")
		}
		return view, nil
	}
	current, err := period.At(policy, now.UTC())
	if err != nil {
		return view, err
	}
	view.PeriodID, view.PeriodStart, view.PeriodEnd = current.ID, current.Start.Format(time.RFC3339Nano), current.End.Format(time.RFC3339Nano)
	view.UsedBytes, view.LastUpdated, err = readTrafficUsageTotalTx(ctx, tx, scope.Key(), current.ID)
	if err != nil {
		return view, err
	}
	view.RemainingBytes, view.Status = quotaStatus(view.LimitBytes, view.UsedBytes)
	if pending {
		view.Accurate = false
		view.Status = append(view.Status, "pending_reconcile")
	}
	return view, nil
}

func trafficReconciliationPendingTx(ctx context.Context, tx *sql.Tx, machineID string) (bool, error) {
	var pending int
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(reconciliation_pending),0) FROM traffic_ledgers WHERE machine_id=?", machineID).Scan(&pending)
	return pending == 1, err
}

func readUserNodeQuotaTx(ctx context.Context, query quotaQuery, userID, nodeID string) (UserNodeQuotaRecord, error) {
	var value sql.NullInt64
	err := query.QueryRowContext(ctx, "SELECT quota_bytes FROM user_node_quotas WHERE user_id=? AND node_id=?", userID, nodeID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return UserNodeQuotaRecord{UserID: userID, NodeID: nodeID}, nil
	}
	if err != nil {
		return UserNodeQuotaRecord{}, err
	}
	record := UserNodeQuotaRecord{UserID: userID, NodeID: nodeID, Exists: true}
	if value.Valid {
		quota := quantity.Bytes(value.Int64)
		record.QuotaBytes = &quota
	}
	return record, nil
}

func readMachineBudgetTx(ctx context.Context, query quotaQuery, machineID string) (MachineBudget, error) {
	var budget, warning sql.NullInt64
	var hardStop bool
	err := query.QueryRowContext(ctx, "SELECT budget_bytes,warning_bytes,hard_stop FROM machine_budgets WHERE machine_id=?", machineID).Scan(&budget, &warning, &hardStop)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineBudget{MachineID: machineID}, nil
	}
	if err != nil {
		return MachineBudget{}, err
	}
	result := MachineBudget{MachineID: machineID, HardStop: hardStop}
	if budget.Valid {
		value := quantity.Bytes(budget.Int64)
		result.BudgetBytes = &value
	}
	if warning.Valid {
		value := quantity.Bytes(warning.Int64)
		result.WarningBytes = &value
	}
	return result, nil
}

func snapshotPolicy(policies []period.Policy, scope period.Scope, now time.Time) (period.Period, bool, error) {
	var selected *period.Policy
	for i := range policies {
		policy := &policies[i]
		if policy.Scope != scope || policy.EffectiveAt.After(now) || selected != nil && !selected.EffectiveAt.Before(policy.EffectiveAt) {
			continue
		}
		selected = policy
	}
	if selected == nil {
		return period.Period{}, false, nil
	}
	current, err := period.At(*selected, now)
	return current, true, err
}

func (s *Store) previousQuotaRestrictionsTx(ctx context.Context, tx *sql.Tx, machineID string) (protocol.QuotaRestrictions, error) {
	var encrypted []byte
	if err := tx.QueryRowContext(ctx, "SELECT desired_snapshot FROM machines WHERE machine_id=?", machineID).Scan(&encrypted); err != nil {
		return protocol.QuotaRestrictions{}, err
	}
	if len(encrypted) == 0 {
		return protocol.QuotaRestrictions{}, nil
	}
	plain, err := security.OpenSecret(s.masterKey, encrypted, "machine-desired:"+machineID)
	if err != nil {
		return protocol.QuotaRestrictions{}, err
	}
	var state protocol.DesiredState
	if err := json.Unmarshal(plain, &state); err != nil {
		return protocol.QuotaRestrictions{}, err
	}
	return state.QuotaRestrictions, state.QuotaRestrictions.Validate()
}

type quotaFence struct {
	CurrentPending bool
	Unknown        bool
	Users          map[string]bool
}

func quotaUsers(state protocol.DesiredState) map[string]bool {
	users := map[string]bool{}
	for _, node := range state.Nodes {
		for _, credential := range node.Credentials {
			if !credential.Development {
				users[credential.UserID] = true
			}
		}
	}
	return users
}

func (s *Store) quotaFenceTx(ctx context.Context, tx *sql.Tx, machineID string, state protocol.DesiredState) (quotaFence, error) {
	fence := quotaFence{Users: map[string]bool{}}
	rows, err := tx.QueryContext(ctx, "SELECT machine_id FROM traffic_ledgers WHERE reconciliation_pending=1 ORDER BY machine_id")
	if err != nil {
		return fence, err
	}
	pendingMachines := []string{}
	for rows.Next() {
		var pendingMachine string
		if err := rows.Scan(&pendingMachine); err != nil {
			rows.Close()
			return fence, err
		}
		pendingMachines = append(pendingMachines, pendingMachine)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fence, err
	}
	rows.Close()
	for _, pendingMachine := range pendingMachines {
		if pendingMachine == machineID {
			fence.CurrentPending = true
			for userID := range quotaUsers(state) {
				fence.Users[userID] = true
			}
			continue
		}
		var encrypted []byte
		if err := tx.QueryRowContext(ctx, "SELECT desired_snapshot FROM machines WHERE machine_id=?", pendingMachine).Scan(&encrypted); err != nil || len(encrypted) == 0 {
			fence.Unknown = true
			continue
		}
		plain, openErr := security.OpenSecret(s.masterKey, encrypted, "machine-desired:"+pendingMachine)
		if openErr != nil {
			fence.Unknown = true
			continue
		}
		var pendingState protocol.DesiredState
		if json.Unmarshal(plain, &pendingState) != nil || protocol.ValidateDesiredState(pendingState) != nil {
			fence.Unknown = true
			continue
		}
		for userID := range quotaUsers(pendingState) {
			fence.Users[userID] = true
		}
	}
	return fence, nil
}

func preserveQuotaRestrictions(previous protocol.QuotaRestrictions, users map[string]bool, nodes map[string]bool, machineStop bool) protocol.QuotaRestrictions {
	preserved := protocol.QuotaRestrictions{MachineStop: machineStop && previous.MachineStop, Items: []protocol.QuotaRestriction{}}
	for _, item := range previous.Items {
		if item.NodeID == "" && users[item.UserID] || item.NodeID != "" && nodes[item.NodeID] && users[item.UserID] {
			preserved.Items = append(preserved.Items, item)
		}
	}
	return preserved
}

func (s *Store) quotaRestrictionsTx(ctx context.Context, tx *sql.Tx, machineID string, state protocol.DesiredState, now time.Time) (protocol.QuotaRestrictions, error) {
	fence, err := s.quotaFenceTx(ctx, tx, machineID, state)
	if err != nil {
		return protocol.QuotaRestrictions{}, err
	}
	users := quotaUsers(state)
	nodes := map[string]bool{}
	for _, node := range state.Nodes {
		nodes[node.ID] = true
	}
	previous, previousErr := s.previousQuotaRestrictionsTx(ctx, tx, machineID)
	if previousErr != nil && !errors.Is(previousErr, sql.ErrNoRows) {
		return protocol.QuotaRestrictions{}, previousErr
	}
	if fence.CurrentPending {
		return preserveQuotaRestrictions(previous, users, nodes, true), nil
	}
	result := protocol.QuotaRestrictions{Items: []protocol.QuotaRestriction{}}
	budget, err := readMachineBudgetTx(ctx, tx, machineID)
	if err != nil {
		return result, err
	}
	if budget.BudgetBytes != nil && budget.HardStop {
		periodValue, found, periodErr := snapshotPolicy(state.PeriodPolicies, period.Scope{Kind: "machine", MachineID: machineID}, now)
		if periodErr != nil {
			return result, periodErr
		}
		if found {
			used, _, usageErr := readTrafficUsageTotalTx(ctx, tx, periodValue.Scope.Key(), periodValue.ID)
			if usageErr != nil {
				return result, usageErr
			}
			if used >= *budget.BudgetBytes {
				result.MachineStop = true
			}
		}
	}
	userIDs := map[string]bool{}
	nodeIDs := map[string]bool{}
	for _, node := range state.Nodes {
		for _, credential := range node.Credentials {
			if credential.Development {
				continue
			}
			userIDs[credential.UserID] = true
			nodeIDs[node.ID] = true
		}
	}
	userKeys := make([]string, 0, len(userIDs))
	for id := range userIDs {
		userKeys = append(userKeys, id)
	}
	sort.Strings(userKeys)
	nodeQuota := map[string]*quantity.Bytes{}
	for nodeID := range nodeIDs {
		var value sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT quota_bytes FROM nodes WHERE node_id=?", nodeID).Scan(&value); err != nil {
			return result, err
		}
		if value.Valid {
			quota := quantity.Bytes(value.Int64)
			nodeQuota[nodeID] = &quota
		}
	}
	for _, userID := range userKeys {
		var global sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT quota_bytes FROM proxy_users WHERE user_id=?", userID).Scan(&global); err != nil {
			return result, err
		}
		if global.Valid {
			quota := quantity.Bytes(global.Int64)
			if current, found, periodErr := snapshotPolicy(state.PeriodPolicies, period.Scope{Kind: "user", UserID: userID}, now); periodErr != nil {
				return result, periodErr
			} else if found {
				used, _, usageErr := readTrafficUsageTotalTx(ctx, tx, current.Scope.Key(), current.ID)
				if usageErr != nil {
					return result, usageErr
				}
				if used >= quota {
					result.Items = append(result.Items, protocol.QuotaRestriction{UserID: userID, Reasons: []string{protocol.RestrictionUserQuotaExhausted}})
				}
			}
		}
		for _, node := range state.Nodes {
			contains := false
			for _, credential := range node.Credentials {
				if credential.UserID == userID && !credential.Development {
					contains = true
					break
				}
			}
			if !contains {
				continue
			}
			override, err := readUserNodeQuotaTx(ctx, tx, userID, node.ID)
			if err != nil {
				return result, err
			}
			limit := nodeQuota[node.ID]
			if override.Exists {
				limit = override.QuotaBytes
			}
			if limit == nil {
				continue
			}
			current, found, periodErr := snapshotPolicy(state.PeriodPolicies, period.Scope{Kind: "user_node", UserID: userID, NodeID: node.ID}, now)
			if periodErr != nil {
				return result, periodErr
			}
			if !found {
				continue
			}
			used, _, usageErr := readTrafficUsageTotalTx(ctx, tx, current.Scope.Key(), current.ID)
			if usageErr != nil {
				return result, usageErr
			}
			if used >= *limit {
				result.Items = append(result.Items, protocol.QuotaRestriction{UserID: userID, NodeID: node.ID, Reasons: []string{protocol.RestrictionNodeQuotaExhausted}})
			}
		}
	}
	if fence.Unknown {
		return preserveQuotaRestrictions(previous, users, nodes, true), nil
	}
	preserved := protocol.QuotaRestrictions{Items: []protocol.QuotaRestriction{}}
	for _, item := range previous.Items {
		if item.NodeID == "" && fence.Users[item.UserID] {
			preserved.Items = append(preserved.Items, item)
		}
	}
	merged, err := protocol.MergeQuotaRestrictions(result, preserved)
	if err != nil {
		return protocol.QuotaRestrictions{}, err
	}
	return merged, merged.Validate()
}

func (s *Store) SaveUserNodeQuota(ctx context.Context, userID, nodeID string, quota *quantity.Bytes, now time.Time) error {
	if !security.IsUUIDv4(userID) || !security.IsUUIDv4(nodeID) || validateQuotaValue(quota) != nil {
		return ErrInvalidQuota
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_users WHERE user_id=? AND deleted_at IS NULL", userID).Scan(&count); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrUserNotFound
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE node_id=? AND deleted_at IS NULL", nodeID).Scan(&count); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrNodeNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_node_quotas(user_id,node_id,quota_bytes,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id,node_id) DO UPDATE SET quota_bytes=excluded.quota_bytes,updated_at=excluded.updated_at`, userID, nodeID, quota, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteUserNodeQuota(ctx context.Context, userID, nodeID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM user_node_quotas WHERE user_id=? AND node_id=?", userID, nodeID); err != nil {
		return err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveMachineBudget(ctx context.Context, budget MachineBudget, now time.Time) error {
	if !security.IsUUIDv4(budget.MachineID) || validateQuotaValue(budget.BudgetBytes) != nil || validateQuotaValue(budget.WarningBytes) != nil || budget.WarningBytes != nil && budget.BudgetBytes != nil && *budget.WarningBytes > *budget.BudgetBytes {
		return ErrInvalidQuota
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM machines WHERE machine_id=?", budget.MachineID).Scan(&count); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrMachineNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO machine_budgets(machine_id,budget_bytes,warning_bytes,hard_stop,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(machine_id) DO UPDATE SET budget_bytes=excluded.budget_bytes,warning_bytes=excluded.warning_bytes,hard_stop=excluded.hard_stop,updated_at=excluded.updated_at`, budget.MachineID, budget.BudgetBytes, budget.WarningBytes, budget.HardStop, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResetQuotaPeriod(ctx context.Context, scope period.Scope, now time.Time) error {
	if scope.Validate() != nil || !period.ValidUTC(now.UTC()) {
		return ErrInvalidQuota
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	var encoded string
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0),COALESCE((SELECT policy_json FROM period_policies WHERE scope_key=? ORDER BY effective_at DESC LIMIT 1),'') FROM period_policies WHERE scope_key=?", scope.Key(), scope.Key()).Scan(&revision, &encoded)
	if err != nil {
		return err
	}
	monthly := period.DefaultMonthly(1)
	if encoded != "" {
		var previous period.Policy
		if err := json.Unmarshal([]byte(encoded), &previous); err != nil || previous.Validate() != nil {
			return ErrInvalidQuota
		}
		monthly = previous.Monthly
	}
	if revision >= 1<<63-1 {
		return ErrInvalidQuota
	}
	policy, err := period.New(scope, uint64(revision+1), now.UTC(), monthly)
	if err != nil {
		return err
	}
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO period_policies(policy_id,scope_key,revision,effective_at,policy_json) VALUES(?,?,?,?,?)", policy.ID, scope.Key(), policy.Revision, policy.EffectiveAt.Format(time.RFC3339Nano), string(encodedPolicy)); err != nil {
		return err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetUserNodeQuota(ctx context.Context, userID, nodeID string) (UserNodeQuotaRecord, error) {
	return readUserNodeQuotaTx(ctx, s.db, userID, nodeID)
}

func (s *Store) GetMachineBudget(ctx context.Context, machineID string) (MachineBudget, error) {
	budget, err := readMachineBudgetTx(ctx, s.db, machineID)
	budget.MachineID = machineID
	return budget, err
}

func (s *Store) GetUserQuotaSummary(ctx context.Context, userID string, now time.Time) (UserQuotaSummary, error) {
	access, err := s.GetUserAccess(ctx, userID, now)
	if err != nil {
		return UserQuotaSummary{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserQuotaSummary{}, err
	}
	defer tx.Rollback()
	pendingGlobal := false
	for _, node := range access.Nodes {
		pending, pendingErr := trafficReconciliationPendingTx(ctx, tx, node.MachineID)
		if pendingErr != nil {
			return UserQuotaSummary{}, pendingErr
		}
		pendingGlobal = pendingGlobal || pending
	}
	global, err := quotaUsageTx(ctx, tx, period.Scope{Kind: "user", UserID: userID}, access.User.QuotaBytes, pendingGlobal, now)
	if err != nil {
		return UserQuotaSummary{}, err
	}
	summary := UserQuotaSummary{UserID: userID, Global: global, Nodes: []UserNodeQuotaView{}, Reasons: append([]string{}, access.UnavailableReasons...)}
	for _, node := range access.Nodes {
		override, err := readUserNodeQuotaTx(ctx, tx, userID, node.NodeID)
		if err != nil {
			return UserQuotaSummary{}, err
		}
		limit := node.QuotaBytes
		if override.Exists {
			limit = override.QuotaBytes
		}
		pending, err := trafficReconciliationPendingTx(ctx, tx, node.MachineID)
		if err != nil {
			return UserQuotaSummary{}, err
		}
		usage, err := quotaUsageTx(ctx, tx, period.Scope{Kind: "user_node", UserID: userID, NodeID: node.NodeID}, limit, pending, now)
		if err != nil {
			return UserQuotaSummary{}, err
		}
		if len(usage.Status) > 0 && usage.Status[0] == "exhausted" {
			summary.Reasons = append(summary.Reasons, "node_quota_exhausted")
		}
		statusText := "无限制"
		if usage.PendingReconcile {
			statusText = "待对账"
		} else if len(usage.Status) > 0 && usage.Status[0] == "exhausted" {
			statusText = "已耗尽"
		} else if len(usage.Status) > 0 && usage.Status[0] == "unknown_policy" {
			statusText = "周期策略待生成"
		}
		summary.Nodes = append(summary.Nodes, UserNodeQuotaView{NodeID: node.NodeID, NodeName: node.Name, MachineID: node.MachineID, DefaultQuota: copyQuota(node.QuotaBytes), HasOverride: override.Exists, OverrideQuota: copyQuota(override.QuotaBytes), EffectiveQuota: copyQuota(limit), Usage: usage, StatusText: statusText})
	}
	if len(global.Status) > 0 && global.Status[0] == "exhausted" {
		summary.Reasons = append(summary.Reasons, "user_quota_exhausted")
	}
	return summary, tx.Commit()
}

func (s *Store) GetMachineQuotaSummary(ctx context.Context, machineID string, now time.Time) (MachineQuotaSummary, error) {
	machine, err := s.GetMachine(ctx, machineID)
	if err != nil {
		return MachineQuotaSummary{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MachineQuotaSummary{}, err
	}
	defer tx.Rollback()
	budget, err := readMachineBudgetTx(ctx, tx, machineID)
	if err != nil {
		return MachineQuotaSummary{}, err
	}
	pending, err := trafficReconciliationPendingTx(ctx, tx, machineID)
	if err != nil {
		return MachineQuotaSummary{}, err
	}
	usage, err := quotaUsageTx(ctx, tx, period.Scope{Kind: "machine", MachineID: machineID}, budget.BudgetBytes, pending, now)
	if err != nil {
		return MachineQuotaSummary{}, err
	}
	softWarning := budget.WarningBytes != nil && usage.UsedBytes >= *budget.WarningBytes && (budget.BudgetBytes == nil || usage.UsedBytes < *budget.BudgetBytes)
	return MachineQuotaSummary{MachineID: machineID, Budget: budget, Usage: usage, SoftWarning: softWarning, RestrictionDigest: machine.AppliedRestrictionDigest, RestrictionAppliedAt: machine.RestrictionAppliedAt}, tx.Commit()
}
