package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrInvalidGroup  = errors.New("组名称或关系无效")
	ErrGroupNotFound = errors.New("组不存在或已删除")
	ErrNameConflict  = errors.New("名称已被使用")
)

type GroupInput struct {
	Name         string   `json:"name"`
	NodeIDs      []string `json:"node_ids,omitempty"`
	NodeGroupIDs []string `json:"node_group_ids,omitempty"`
}

type GroupRecord struct {
	GroupID string `json:"group_id"`
	GroupInput
}

func groupTable(kind string) (string, error) {
	switch kind {
	case "user":
		return "user_groups", nil
	case "node":
		return "node_groups", nil
	}
	return "", ErrInvalidGroup
}

func relationIDs(ids []string) ([]string, error) {
	if len(ids) > 1000 {
		return nil, ErrInvalidGroup
	}
	seen := map[string]bool{}
	result := []string{}
	for _, id := range ids {
		if !security.IsUUIDv4(id) {
			return nil, ErrInvalidGroup
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) SaveGroup(ctx context.Context, kind, id string, input GroupInput, now time.Time) (GroupRecord, error) {
	table, err := groupTable(kind)
	if err != nil {
		return GroupRecord{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if !protocol.ValidText(input.Name, 128) || kind == "user" && len(input.NodeIDs) > 0 || kind == "node" && len(input.NodeGroupIDs) > 0 {
		return GroupRecord{}, ErrInvalidGroup
	}
	ids := input.NodeGroupIDs
	if kind == "node" {
		ids = input.NodeIDs
	}
	ids, err = relationIDs(ids)
	if err != nil {
		return GroupRecord{}, err
	}
	if kind == "node" {
		input.NodeIDs = ids
	} else {
		input.NodeGroupIDs = ids
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GroupRecord{}, err
	}
	defer tx.Rollback()
	var exists int
	if id != "" {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE group_id = ? AND deleted_at IS NULL", id).Scan(&exists); err != nil {
			return GroupRecord{}, err
		}
		if exists != 1 {
			return GroupRecord{}, ErrGroupNotFound
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE name_key = ? AND group_id != ? AND deleted_at IS NULL", strings.ToLower(input.Name), id).Scan(&exists); err != nil {
		return GroupRecord{}, err
	}
	if exists != 0 {
		return GroupRecord{}, ErrNameConflict
	}
	for _, member := range ids {
		query := "SELECT COUNT(*) FROM node_groups WHERE group_id = ? AND deleted_at IS NULL"
		if kind == "node" {
			query = "SELECT COUNT(*) FROM nodes WHERE node_id = ? AND deleted_at IS NULL"
		}
		if err := tx.QueryRowContext(ctx, query, member).Scan(&exists); err != nil {
			return GroupRecord{}, err
		}
		if exists != 1 {
			return GroupRecord{}, ErrGroupNotFound
		}
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	if id == "" {
		id, err = security.NewUUID()
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(group_id,name,name_key,created_at,updated_at) VALUES(?,?,?,?,?)", id, input.Name, strings.ToLower(input.Name), nowText, nowText)
		}
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET name = ?, name_key = ?, updated_at = ? WHERE group_id = ?", input.Name, strings.ToLower(input.Name), nowText, id)
	}
	if err != nil {
		return GroupRecord{}, err
	}
	if kind == "node" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM node_group_nodes WHERE node_group_id = ?", id); err != nil {
			return GroupRecord{}, err
		}
		for _, member := range ids {
			if _, err = tx.ExecContext(ctx, "INSERT INTO node_group_nodes(node_group_id,node_id) VALUES(?,?)", id, member); err != nil {
				return GroupRecord{}, err
			}
		}
	} else {
		if _, err = tx.ExecContext(ctx, "DELETE FROM user_group_node_groups WHERE user_group_id = ?", id); err != nil {
			return GroupRecord{}, err
		}
		for _, member := range ids {
			if _, err = tx.ExecContext(ctx, "INSERT INTO user_group_node_groups(user_group_id,node_group_id) VALUES(?,?)", id, member); err != nil {
				return GroupRecord{}, err
			}
		}
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return GroupRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return GroupRecord{}, err
	}
	return GroupRecord{GroupID: id, GroupInput: input}, nil
}

func (s *Store) DeleteGroup(ctx context.Context, kind, id string, now time.Time) error {
	table, err := groupTable(kind)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET deleted_at = ?, updated_at = ? WHERE group_id = ? AND deleted_at IS NULL", now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrGroupNotFound
	}
	if err := s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListGroups(ctx context.Context, kind string) ([]GroupRecord, error) {
	table, err := groupTable(kind)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT group_id,name FROM "+table+" WHERE deleted_at IS NULL ORDER BY name_key,group_id")
	if err != nil {
		return nil, err
	}
	groups := []GroupRecord{}
	for rows.Next() {
		var group GroupRecord
		if err := rows.Scan(&group.GroupID, &group.Name); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range groups {
		query := `SELECT r.node_group_id FROM user_group_node_groups r JOIN node_groups g ON g.group_id=r.node_group_id WHERE r.user_group_id=? AND g.deleted_at IS NULL ORDER BY r.node_group_id`
		if kind == "node" {
			query = `SELECT r.node_id FROM node_group_nodes r JOIN nodes n ON n.node_id=r.node_id WHERE r.node_group_id=? AND n.deleted_at IS NULL ORDER BY r.node_id`
		}
		rows, err := tx.QueryContext(ctx, query, groups[i].GroupID)
		if err != nil {
			return nil, err
		}
		members := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			members = append(members, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if kind == "node" {
			groups[i].NodeIDs = members
		} else {
			groups[i].NodeGroupIDs = members
		}
	}
	return groups, tx.Commit()
}

func (s *Store) refreshAllMachineDesiredTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.machine_id
		FROM machines m LEFT JOIN exits e ON e.machine_id=m.machine_id
		ORDER BY CASE WHEN e.machine_id IS NULL THEN 1 ELSE 0 END, m.machine_id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.refreshMachineDesiredTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
