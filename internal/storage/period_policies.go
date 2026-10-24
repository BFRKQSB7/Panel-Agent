package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

// A D06 database already has encrypted snapshots. Merely adding the policy
// table must not leave those Agents on a permanently unchanged legacy snapshot.
func (s *Store) ensureMachinePeriodSnapshots(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT machine_id,desired_snapshot FROM machines ORDER BY machine_id")
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted); err != nil {
			rows.Close()
			return err
		}
		if len(encrypted) == 0 {
			continue
		} // Unconfigured D03 enrollment keeps its empty snapshot.
		if len(encrypted) > 0 {
			plain, err := security.OpenSecret(s.masterKey, encrypted, "machine-desired:"+id)
			if err != nil {
				rows.Close()
				return err
			}
			var state protocol.DesiredState
			if err = json.Unmarshal(plain, &state); err != nil {
				rows.Close()
				return err
			}
			if len(state.PeriodPolicies) > 0 {
				continue
			}
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.refreshMachineDesiredTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Default policies are stored once per stable scope, not minted per machine.
// Later quota administration appends immutable policies to the same history.
func machinePoliciesTx(ctx context.Context, tx *sql.Tx, machineID string, state protocol.DesiredState) ([]period.Policy, error) {
	scopes := map[string]period.Scope{}
	add := func(s period.Scope) { scopes[s.Key()] = s }
	add(period.Scope{Kind: "machine", MachineID: machineID})
	for _, node := range state.Nodes {
		for _, c := range node.Credentials {
			if c.Development {
				continue
			}
			add(period.Scope{Kind: "user", UserID: c.UserID})
			add(period.Scope{Kind: "user_node", UserID: c.UserID, NodeID: node.ID})
		}
	}
	keys := make([]string, 0, len(scopes))
	for k := range scopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	policies := []period.Policy{}
	for _, key := range keys {
		rows, err := tx.QueryContext(ctx, "SELECT policy_json FROM period_policies WHERE scope_key=? ORDER BY effective_at,policy_id", key)
		if err != nil {
			return nil, err
		}
		found := false
		for rows.Next() {
			var encoded string
			if err := rows.Scan(&encoded); err != nil {
				rows.Close()
				return nil, err
			}
			var p period.Policy
			if err := json.Unmarshal([]byte(encoded), &p); err != nil {
				rows.Close()
				return nil, err
			}
			if err := p.Validate(); err != nil {
				rows.Close()
				return nil, err
			}
			policies = append(policies, p)
			found = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if found {
			continue
		}
		p, err := period.New(scopes[key], 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO period_policies(policy_id,scope_key,revision,effective_at,policy_json) VALUES(?,?,?,?,?)", p.ID, key, p.Revision, p.EffectiveAt.Format(time.RFC3339Nano), string(encoded)); err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	return policies, nil
}
