package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrInvalidUser  = errors.New("用户字段无效")
	ErrUserNotFound = errors.New("用户不存在或已删除")
)

type UserInput struct {
	Name        string          `json:"name"`
	UserGroupID string          `json:"user_group_id"`
	Enabled     bool            `json:"enabled"`
	ExpiresAt   string          `json:"expires_at"`
	QuotaBytes  *quantity.Bytes `json:"quota_bytes"`
}

type UserRecord struct {
	UserID string `json:"user_id"`
	UserInput
	GroupName            string `json:"group_name"`
	GroupDeleted         bool   `json:"group_deleted"`
	CredentialUUID       string `json:"credential_uuid"`
	CredentialGeneration uint64 `json:"credential_generation"`
}

func (u UserRecord) UnavailableReasons(now time.Time) []string {
	reasons := []string{}
	if !u.Enabled {
		reasons = append(reasons, "manually_disabled")
	}
	if u.GroupDeleted {
		reasons = append(reasons, "group_deleted")
	}
	if u.ExpiresAt != "" {
		if expiry, err := time.Parse(time.RFC3339, u.ExpiresAt); err != nil || !now.Before(expiry) {
			reasons = append(reasons, "expired")
		}
	}
	return reasons
}

func normalizeUser(input UserInput) (UserInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if !protocol.ValidText(input.Name, 128) || !security.IsUUIDv4(input.UserGroupID) || input.QuotaBytes != nil && *input.QuotaBytes < 0 {
		return input, ErrInvalidUser
	}
	if input.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ExpiresAt))
		if err != nil || expiry.Year() < 1970 || expiry.Year() > 9999 {
			return input, ErrInvalidUser
		}
		input.ExpiresAt = expiry.UTC().Format(time.RFC3339)
	}
	return input, nil
}

func userCredentialOwner(userID string) string { return "user-credential:" + userID + ":vless" }

func (s *Store) SaveUser(ctx context.Context, id string, input UserInput, now time.Time) (UserRecord, error) {
	input, err := normalizeUser(input)
	if err != nil {
		return UserRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserRecord{}, err
	}
	defer tx.Rollback()
	var count int
	if id != "" {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_users WHERE user_id=? AND deleted_at IS NULL", id).Scan(&count); err != nil {
			return UserRecord{}, err
		}
		if count != 1 {
			return UserRecord{}, ErrUserNotFound
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_groups WHERE group_id=? AND deleted_at IS NULL", input.UserGroupID).Scan(&count); err != nil {
		return UserRecord{}, err
	}
	if count != 1 {
		return UserRecord{}, ErrGroupNotFound
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_users WHERE name_key=? AND user_id!=? AND deleted_at IS NULL", strings.ToLower(input.Name), id).Scan(&count); err != nil {
		return UserRecord{}, err
	}
	if count != 0 {
		return UserRecord{}, ErrNameConflict
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	var expiry any
	if input.ExpiresAt != "" {
		expiry = input.ExpiresAt
	}
	if id == "" {
		id, err = security.NewUUID()
		if err != nil {
			return UserRecord{}, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO proxy_users(user_id,name,name_key,user_group_id,enabled,expires_at,quota_bytes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", id, input.Name, strings.ToLower(input.Name), input.UserGroupID, input.Enabled, expiry, input.QuotaBytes, nowText, nowText)
		if err != nil {
			return UserRecord{}, err
		}
		uuid, err := security.NewUUID()
		if err != nil {
			return UserRecord{}, err
		}
		encrypted, err := security.SealSecret(s.masterKey, []byte(uuid), userCredentialOwner(id))
		if err != nil {
			return UserRecord{}, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO proxy_credentials(user_id,protocol,encrypted_secret,generation,updated_at) VALUES(?,'vless',?,1,?)", id, encrypted, nowText)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE proxy_users SET name=?,name_key=?,user_group_id=?,enabled=?,expires_at=?,quota_bytes=?,updated_at=? WHERE user_id=?", input.Name, strings.ToLower(input.Name), input.UserGroupID, input.Enabled, expiry, input.QuotaBytes, nowText, id)
	}
	if err != nil {
		return UserRecord{}, err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return UserRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return UserRecord{}, err
	}
	return s.GetUser(ctx, id)
}

const userSelect = `SELECT u.user_id,u.name,u.user_group_id,u.enabled,u.expires_at,u.quota_bytes,g.name,g.deleted_at,c.encrypted_secret,c.generation
 FROM proxy_users u JOIN user_groups g ON g.group_id=u.user_group_id JOIN proxy_credentials c ON c.user_id=u.user_id AND c.protocol='vless'`

func (s *Store) scanUser(row rowScanner) (UserRecord, error) {
	var user UserRecord
	var expiry, groupDeleted sql.NullString
	var quota sql.NullInt64
	var encrypted []byte
	var generation int64
	err := row.Scan(&user.UserID, &user.Name, &user.UserGroupID, &user.Enabled, &expiry, &quota, &user.GroupName, &groupDeleted, &encrypted, &generation)
	if err != nil {
		return UserRecord{}, err
	}
	if generation < 1 {
		return UserRecord{}, ErrInvalidUser
	}
	secret, err := security.OpenSecret(s.masterKey, encrypted, userCredentialOwner(user.UserID))
	if err != nil {
		return UserRecord{}, err
	}
	if !security.IsUUIDv4(string(secret)) {
		return UserRecord{}, ErrInvalidUser
	}
	user.ExpiresAt, user.GroupDeleted, user.CredentialGeneration, user.CredentialUUID = expiry.String, groupDeleted.Valid, uint64(generation), string(secret)
	if quota.Valid {
		value := quantity.Bytes(quota.Int64)
		user.QuotaBytes = &value
	}
	return user, nil
}

func (s *Store) GetUser(ctx context.Context, id string) (UserRecord, error) {
	user, err := s.scanUser(s.db.QueryRowContext(ctx, userSelect+" WHERE u.user_id=? AND u.deleted_at IS NULL", id))
	if errors.Is(err, sql.ErrNoRows) {
		return UserRecord{}, ErrUserNotFound
	}
	return user, err
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+" WHERE u.deleted_at IS NULL ORDER BY u.name_key,u.user_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []UserRecord{}
	for rows.Next() {
		user, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) RotateUserCredential(ctx context.Context, id string, now time.Time) (UserRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserRecord{}, err
	}
	defer tx.Rollback()
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT c.generation FROM proxy_credentials c JOIN proxy_users u ON u.user_id=c.user_id WHERE c.user_id=? AND c.protocol='vless' AND u.deleted_at IS NULL", id).Scan(&generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, ErrUserNotFound
		}
		return UserRecord{}, err
	}
	if generation == 1<<63-1 {
		return UserRecord{}, errors.New("credential generation exhausted")
	}
	uuid, err := security.NewUUID()
	if err != nil {
		return UserRecord{}, err
	}
	encrypted, err := security.SealSecret(s.masterKey, []byte(uuid), userCredentialOwner(id))
	if err != nil {
		return UserRecord{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE proxy_credentials SET encrypted_secret=?,generation=generation+1,updated_at=? WHERE user_id=? AND protocol='vless'", encrypted, now.UTC().Format(time.RFC3339Nano), id); err != nil {
		return UserRecord{}, err
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return UserRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return UserRecord{}, err
	}
	return s.GetUser(ctx, id)
}

func (s *Store) DeleteUser(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE proxy_users SET deleted_at=?,updated_at=? WHERE user_id=? AND deleted_at IS NULL", now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrUserNotFound
	}
	if err = s.refreshAllMachineDesiredTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type UserAccess struct {
	User               UserRecord   `json:"user"`
	Nodes              []NodeRecord `json:"nodes"`
	UnavailableReasons []string     `json:"unavailable_reasons"`
}

func (s *Store) GetUserAccess(ctx context.Context, id string, now time.Time) (UserAccess, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserAccess{}, err
	}
	defer tx.Rollback()
	user, err := s.scanUser(tx.QueryRowContext(ctx, userSelect+" WHERE u.user_id=? AND u.deleted_at IS NULL", id))
	if errors.Is(err, sql.ErrNoRows) {
		return UserAccess{}, ErrUserNotFound
	}
	if err != nil {
		return UserAccess{}, err
	}
	access := UserAccess{User: user, Nodes: []NodeRecord{}, UnavailableReasons: user.UnavailableReasons(now)}
	if len(access.UnavailableReasons) > 0 {
		return access, tx.Commit()
	}
	// Only the group path grants access. Heartbeat and Machine connectivity do
	// not remove definitions, and there are no per-User per-Node override rows.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT n.node_id FROM nodes n
 JOIN node_group_nodes nn ON nn.node_id=n.node_id
 JOIN node_groups ng ON ng.group_id=nn.node_group_id AND ng.deleted_at IS NULL
 JOIN user_group_node_groups un ON un.node_group_id=ng.group_id
 WHERE un.user_group_id=? AND n.deleted_at IS NULL AND n.enabled=1 ORDER BY n.node_id`, user.UserGroupID)
	if err != nil {
		return UserAccess{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return UserAccess{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return UserAccess{}, err
	}
	for _, nodeID := range ids {
		node, err := scanNode(tx.QueryRowContext(ctx, nodeSelect+" WHERE node_id=?", nodeID))
		if err != nil {
			return UserAccess{}, err
		}
		access.Nodes = append(access.Nodes, node)
	}
	return access, tx.Commit()
}

type UserBatchInput struct {
	Operation   string          `json:"operation"`
	UserIDs     []string        `json:"user_ids"`
	UserGroupID string          `json:"user_group_id,omitempty"`
	ExpiresAt   string          `json:"expires_at,omitempty"`
	QuotaBytes  *quantity.Bytes `json:"quota_bytes"`
}

func (s *Store) ChangeUsers(ctx context.Context, input UserBatchInput, now time.Time) ([]BatchUserResult, error) {
	if len(input.UserIDs) == 0 || len(input.UserIDs) > 100 {
		return nil, ErrInvalidUser
	}
	switch input.Operation {
	case "move", "enable", "disable", "expire", "quota", "rotate", "delete":
	default:
		return nil, ErrInvalidUser
	}
	seen := map[string]bool{}
	results := []BatchUserResult{}
	for i, id := range input.UserIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		result := BatchUserResult{Index: i + 1, UserID: id}
		user, err := s.GetUser(ctx, id)
		if err == nil {
			result.Name = user.Name
			switch input.Operation {
			case "rotate":
				_, err = s.RotateUserCredential(ctx, id, now)
			case "delete":
				err = s.DeleteUser(ctx, id, now)
			default:
				change := user.UserInput
				switch input.Operation {
				case "move":
					change.UserGroupID = input.UserGroupID
				case "enable":
					change.Enabled = true
				case "disable":
					change.Enabled = false
				case "expire":
					change.ExpiresAt = input.ExpiresAt
				case "quota":
					change.QuotaBytes = input.QuotaBytes
				}
				_, err = s.SaveUser(ctx, id, change, now)
			}
		}
		if err != nil {
			result.Error = userErrorCode(err)
		}
		results = append(results, result)
	}
	return results, nil
}

func userErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrUserNotFound):
		return "user_not_found"
	case errors.Is(err, ErrNameConflict):
		return "name_conflict"
	case errors.Is(err, ErrGroupNotFound):
		return "group_not_found"
	case errors.Is(err, ErrInvalidUser):
		return "invalid_user"
	}
	return "storage_failed"
}

type BatchUserResult struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	UserID string `json:"user_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Store) CreateUsers(ctx context.Context, inputs []UserInput, now time.Time) ([]BatchUserResult, error) {
	if len(inputs) == 0 || len(inputs) > 100 {
		return nil, ErrInvalidUser
	}
	results := make([]BatchUserResult, 0, len(inputs))
	for i, input := range inputs {
		result := BatchUserResult{Index: i + 1, Name: input.Name}
		user, err := s.SaveUser(ctx, "", input, now)
		if err == nil {
			result.UserID = user.UserID
		} else {
			result.Error = userErrorCode(err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Store) nodeCredentialsTx(ctx context.Context, tx *sql.Tx, nodeID string) ([]protocol.ProxyCredential, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT u.user_id,c.encrypted_secret,u.expires_at
 FROM proxy_users u JOIN user_groups ug ON ug.group_id=u.user_group_id AND ug.deleted_at IS NULL
 JOIN user_group_node_groups un ON un.user_group_id=ug.group_id
 JOIN node_groups ng ON ng.group_id=un.node_group_id AND ng.deleted_at IS NULL
 JOIN node_group_nodes nn ON nn.node_group_id=ng.group_id
 JOIN proxy_credentials c ON c.user_id=u.user_id AND c.protocol='vless'
 WHERE nn.node_id=? AND u.deleted_at IS NULL AND u.enabled=1 ORDER BY u.user_id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := []protocol.ProxyCredential{}
	for rows.Next() {
		var id string
		var encrypted []byte
		var expiry sql.NullString
		if err := rows.Scan(&id, &encrypted, &expiry); err != nil {
			return nil, err
		}
		secret, err := security.OpenSecret(s.masterKey, encrypted, userCredentialOwner(id))
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, protocol.ProxyCredential{UserID: id, UUID: string(secret), Email: "u-" + id + "-n-" + nodeID, ExpiresAt: expiry.String})
	}
	return credentials, rows.Err()
}
