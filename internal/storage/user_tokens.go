package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"panel-agent.local/panel-agent/internal/security"
)

var ErrCapabilityNotFound = errors.New("链接无效或已撤销")
var ErrCapabilityConflict = errors.New("Token 已被使用，不能重复或保留旧值")

type UserTokenState struct {
	UserID                 string `json:"user_id"`
	DashboardGeneration    uint64 `json:"dashboard_generation"`
	SubscriptionGeneration uint64 `json:"subscription_generation"`
}

type TokenMutation struct {
	UserID string                  `json:"user_id"`
	Spec   security.CapabilitySpec `json:"spec"`
}

type TokenDelivery struct {
	UserID         string `json:"user_id"`
	Kind           string `json:"kind"`
	Token          string `json:"token,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	AlreadyApplied bool   `json:"already_applied,omitempty"`
	Error          string `json:"error,omitempty"`
}

func validTokenKind(kind string) bool { return kind == "dashboard" || kind == "subscription" }

func (s *Store) GetUserTokenState(ctx context.Context, id string) (UserTokenState, error) {
	if _, err := s.GetUser(ctx, id); err != nil {
		return UserTokenState{}, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT kind,generation FROM user_tokens WHERE user_id=? AND revoked_at IS NULL", id)
	if err != nil {
		return UserTokenState{}, err
	}
	defer rows.Close()
	state := UserTokenState{UserID: id}
	for rows.Next() {
		var kind string
		var generation int64
		if err := rows.Scan(&kind, &generation); err != nil {
			return UserTokenState{}, err
		}
		if generation < 1 {
			return UserTokenState{}, ErrCapabilityNotFound
		}
		if kind == "dashboard" {
			state.DashboardGeneration = uint64(generation)
		} else {
			state.SubscriptionGeneration = uint64(generation)
		}
	}
	return state, rows.Err()
}

func (s *Store) UserForCapability(ctx context.Context, kind, value string) (string, error) {
	if !validTokenKind(kind) || !security.CapabilitySyntax(value) {
		return "", ErrCapabilityNotFound
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT t.user_id FROM user_tokens t JOIN proxy_users u ON u.user_id=t.user_id WHERE t.kind=? AND t.token_hash=? AND t.revoked_at IS NULL AND u.deleted_at IS NULL`, kind, security.HashToken(value)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCapabilityNotFound
	}
	return id, err
}

// The HTTP layer verifies a signed, short-lived operation scope first. Success
// receipts survive process restarts; retries never rotate successful items or
// pretend their one-time plaintext can be recovered from the stored hash.
func (s *Store) ResetUserToken(ctx context.Context, operationID, kind string, input TokenMutation, now time.Time) (TokenDelivery, error) {
	if !security.IsUUIDv4(operationID) || !validTokenKind(kind) {
		return TokenDelivery{}, ErrInvalidUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TokenDelivery{}, err
	}
	defer tx.Rollback()
	delivery := TokenDelivery{UserID: input.UserID, Kind: kind}
	var prior int64
	err = tx.QueryRowContext(ctx, "SELECT generation FROM token_operations WHERE operation_id=? AND user_id=? AND kind=?", operationID, input.UserID, kind).Scan(&prior)
	if err == nil {
		delivery.Generation = uint64(prior)
		delivery.AlreadyApplied = true
		return delivery, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TokenDelivery{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_users WHERE user_id=? AND deleted_at IS NULL", input.UserID).Scan(&count); err != nil {
		return TokenDelivery{}, err
	}
	if count != 1 {
		return TokenDelivery{}, ErrUserNotFound
	}
	value, err := security.NewCapability(input.Spec)
	if err != nil {
		return TokenDelivery{}, err
	}
	hash := security.HashToken(value)
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_tokens WHERE token_hash=?", hash).Scan(&count); err != nil {
		return TokenDelivery{}, err
	}
	if count != 0 {
		return TokenDelivery{}, ErrCapabilityConflict
	}
	var generation int64
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(generation),0) FROM user_tokens WHERE user_id=? AND kind=?", input.UserID, kind).Scan(&generation)
	if err != nil {
		return TokenDelivery{}, err
	}
	if generation == 1<<63-1 {
		return TokenDelivery{}, errors.New("Token generation exhausted")
	}
	generation++
	nowText := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE user_tokens SET revoked_at=? WHERE user_id=? AND kind=? AND revoked_at IS NULL", nowText, input.UserID, kind); err != nil {
		return TokenDelivery{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_tokens(user_id,kind,token_hash,generation,updated_at) VALUES(?,?,?,?,?)`, input.UserID, kind, hash, generation, nowText)
	if err != nil {
		return TokenDelivery{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO token_operations(operation_id,user_id,kind,token_hash,generation,created_at) VALUES(?,?,?,?,?,?)", operationID, input.UserID, kind, hash, generation, nowText); err != nil {
		return TokenDelivery{}, err
	}
	// Signed actions expire after 15 minutes; retaining receipts for a day covers
	// all still-valid retries without accumulating plaintext or unbounded history.
	if _, err = tx.ExecContext(ctx, "DELETE FROM token_operations WHERE created_at<?", now.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		return TokenDelivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return TokenDelivery{}, err
	}
	delivery.Token, delivery.Generation = value, uint64(generation)
	return delivery, nil
}

func (s *Store) ResetUserTokens(ctx context.Context, operationID, kind string, items []TokenMutation, now time.Time) ([]TokenDelivery, error) {
	if len(items) == 0 || len(items) > 100 || !security.IsUUIDv4(operationID) || !validTokenKind(kind) {
		return nil, ErrInvalidUser
	}
	results := make([]TokenDelivery, 0, len(items))
	for _, item := range items {
		delivery, err := s.ResetUserToken(ctx, operationID, kind, item, now)
		if err != nil {
			delivery = TokenDelivery{UserID: item.UserID, Kind: kind}
			switch {
			case errors.Is(err, security.ErrWeakCapability):
				delivery.Error = "weak_token"
			case errors.Is(err, ErrCapabilityConflict):
				delivery.Error = "token_conflict"
			default:
				delivery.Error = userErrorCode(err)
			}
		}
		results = append(results, delivery)
	}
	return results, nil
}
