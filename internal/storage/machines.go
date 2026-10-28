package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrInvalidJoinToken        = errors.New("invalid Join Token")
	ErrExpiredJoinToken        = errors.New("Join Token expired")
	ErrUsedJoinToken           = errors.New("Join Token was already used")
	ErrRegistrationConflict    = errors.New("registration ID is already in use")
	ErrMachineAuthentication   = errors.New("machine authentication failed")
	ErrMachineNotFound         = errors.New("machine not found")
	ErrAuthorityMismatch       = errors.New("machine authority does not match")
	ErrStaleRevision           = errors.New("desired state revision is stale or unknown")
	ErrDigestMismatch          = errors.New("desired state digest does not match revision")
	ErrCoreNotImplemented      = errors.New("Core application is not implemented")
	ErrDesiredStateNotReceived = errors.New("desired state has not been received")
)

type PanelAuthority struct {
	Epoch    string
	Revision uint64
	Digest   string
	Snapshot protocol.DesiredState
}

type MachineRecord struct {
	MachineID                string
	RegistrationID           string
	TokenHash                string
	Hostname                 string
	DisplayName              string
	Notes                    string
	OS                       string
	Architecture             string
	CPUCount                 int
	MemoryBytes              uint64
	AgentVersion             string
	AuthorityEpoch           string
	DesiredRevision          uint64
	DesiredDigest            string
	ReceivedRevision         uint64
	ReceivedDigest           string
	AppliedRevision          uint64
	AppliedDigest            string
	AppliedAuthorityEpoch    string
	CoreGeneration           uint64
	CoreInstanceID           string
	AppliedRestrictionDigest string
	RestrictionAppliedAt     string
	ApplyError               string
	PendingAuthorityEpoch    string
	PendingAuthorityNonce    string
	LastAuthorityNonce       string
	LastHeartbeatAt          string
	Revoked                  bool
	CreatedAt                string
	UpdatedAt                string
}

type RegisterMachineInput struct {
	RegistrationID  string
	JoinTokenHash   string
	CandidateHash   string
	TargetMachineID string
	Hostname        string
	DisplayName     string
	OS              string
	Architecture    string
	CPUCount        int
	MemoryBytes     uint64
	AgentVersion    string
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) GetPanelAuthority(ctx context.Context) (PanelAuthority, error) {
	return readPanelAuthority(ctx, s.db)
}

func readPanelAuthority(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (PanelAuthority, error) {
	var authority PanelAuthority
	var revision int64
	var snapshotJSON string
	err := query.QueryRowContext(ctx, "SELECT authority_epoch, revision, digest, snapshot_json FROM panel_authority WHERE id = 1").Scan(&authority.Epoch, &revision, &authority.Digest, &snapshotJSON)
	if err != nil {
		return PanelAuthority{}, fmt.Errorf("read Panel authority: %w", err)
	}
	if revision <= 0 {
		return PanelAuthority{}, errors.New("Panel authority has invalid revision")
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &authority.Snapshot); err != nil {
		return PanelAuthority{}, fmt.Errorf("decode Panel desired state: %w", err)
	}
	calculatedDigest, err := protocol.DigestDesiredState(authority.Snapshot)
	if err != nil {
		return PanelAuthority{}, fmt.Errorf("verify Panel desired state: %w", err)
	}
	if calculatedDigest != authority.Digest {
		return PanelAuthority{}, errors.New("Panel desired state digest is inconsistent")
	}
	authority.Revision = uint64(revision)
	return authority, nil
}

func (s *Store) RotatePanelAuthority(ctx context.Context, now time.Time) (PanelAuthority, error) {
	epoch, err := security.NewUUID()
	if err != nil {
		return PanelAuthority{}, err
	}
	snapshot := protocol.EmptyDesiredState()
	digest, err := protocol.DigestDesiredState(snapshot)
	if err != nil {
		return PanelAuthority{}, err
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return PanelAuthority{}, fmt.Errorf("encode desired state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE panel_authority SET authority_epoch = ?, revision = 1, digest = ?, snapshot_json = ?, updated_at = ? WHERE id = 1", epoch, digest, string(snapshotJSON), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return PanelAuthority{}, fmt.Errorf("rotate Panel authority: %w", err)
	}
	return s.GetPanelAuthority(ctx)
}

func (s *Store) CreateJoinToken(ctx context.Context, tokenHash, targetMachineID string, expiresAt, now time.Time) error {
	var target any
	if targetMachineID != "" {
		var exists int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM machines WHERE machine_id = ?", targetMachineID).Scan(&exists); err != nil {
			return fmt.Errorf("check re-pair target: %w", err)
		}
		if exists == 0 {
			return ErrMachineNotFound
		}
		target = targetMachineID
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO join_tokens(token_hash, target_machine_id, created_at, expires_at) VALUES(?, ?, ?, ?)", tokenHash, target, now.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("persist Join Token: %w", err)
	}
	return nil
}

func (s *Store) RegisterMachine(ctx context.Context, input RegisterMachineInput, now time.Time) (MachineRecord, PanelAuthority, error) {
	if input.RegistrationID == "" || input.JoinTokenHash == "" || input.CandidateHash == "" || input.Hostname == "" || input.OS == "" || input.Architecture == "" {
		return MachineRecord{}, PanelAuthority{}, ErrInvalidJoinToken
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("begin machine registration: %w", err)
	}
	defer tx.Rollback()

	var existingMachineID string
	err = tx.QueryRowContext(ctx, "SELECT machine_id FROM machines WHERE registration_id = ?", input.RegistrationID).Scan(&existingMachineID)
	if err == nil {
		return MachineRecord{}, PanelAuthority{}, ErrRegistrationConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("check registration identity: %w", err)
	}

	var tokenExpiry string
	var consumedAt sql.NullString
	var targetMachine sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT expires_at, consumed_at, target_machine_id FROM join_tokens WHERE token_hash = ?", input.JoinTokenHash).Scan(&tokenExpiry, &consumedAt, &targetMachine)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRecord{}, PanelAuthority{}, ErrInvalidJoinToken
	}
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("read Join Token: %w", err)
	}
	if consumedAt.Valid {
		return MachineRecord{}, PanelAuthority{}, ErrUsedJoinToken
	}
	expires, err := time.Parse(time.RFC3339Nano, tokenExpiry)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("parse Join Token expiration: %w", err)
	}
	if !now.Before(expires) {
		return MachineRecord{}, PanelAuthority{}, ErrExpiredJoinToken
	}
	if targetMachine.String != input.TargetMachineID {
		return MachineRecord{}, PanelAuthority{}, ErrInvalidJoinToken
	}

	panelAuthority, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, err
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	machineID := input.TargetMachineID
	if machineID == "" {
		machineID, err = security.NewUUID()
		if err != nil {
			return MachineRecord{}, PanelAuthority{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO machines(
			machine_id, registration_id, token_hash, hostname, display_name, os, architecture,
			cpu_count, memory_bytes, agent_version, authority_epoch, desired_revision, desired_digest,
			last_heartbeat_at, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			machineID, input.RegistrationID, input.CandidateHash, input.Hostname, displayName(input.DisplayName, input.Hostname),
			input.OS, input.Architecture, input.CPUCount, input.MemoryBytes, input.AgentVersion,
			panelAuthority.Epoch, panelAuthority.Revision, panelAuthority.Digest, nowText, nowText, nowText)
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE machines SET
			registration_id = ?, token_hash = ?, hostname = ?,
			display_name = CASE WHEN ? = '' THEN display_name ELSE ? END,
			os = ?, architecture = ?, cpu_count = ?, memory_bytes = ?, agent_version = ?,
			last_heartbeat_at = ?, revoked_at = NULL, updated_at = ?
			WHERE machine_id = ?`,
			input.RegistrationID, input.CandidateHash, input.Hostname, input.DisplayName, input.DisplayName,
			input.OS, input.Architecture, input.CPUCount, input.MemoryBytes, input.AgentVersion, nowText, nowText, machineID)
		if updateErr != nil {
			return MachineRecord{}, PanelAuthority{}, fmt.Errorf("re-pair machine: %w", updateErr)
		}
		count, updateErr := result.RowsAffected()
		if updateErr != nil || count != 1 {
			if updateErr != nil {
				return MachineRecord{}, PanelAuthority{}, fmt.Errorf("verify re-paired machine: %w", updateErr)
			}
			return MachineRecord{}, PanelAuthority{}, ErrMachineNotFound
		}
	}
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("persist machine registration: %w", err)
	}
	result, err := tx.ExecContext(ctx, "UPDATE join_tokens SET consumed_at = ? WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?", nowText, input.JoinTokenHash, nowText)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("consume Join Token: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return MachineRecord{}, PanelAuthority{}, fmt.Errorf("verify Join Token consumption: %w", err)
		}
		return MachineRecord{}, PanelAuthority{}, ErrUsedJoinToken
	}
	if err := tx.Commit(); err != nil {
		return MachineRecord{}, PanelAuthority{}, fmt.Errorf("commit machine registration: %w", err)
	}
	machine, err := s.GetMachine(ctx, machineID)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, err
	}
	current, err := s.GetMachineDesiredState(ctx, machineID)
	if err != nil {
		return MachineRecord{}, PanelAuthority{}, err
	}
	return machine, current, nil
}

func displayName(name, hostname string) string {
	if strings.TrimSpace(name) == "" {
		return hostname
	}
	return strings.TrimSpace(name)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) RecoverRegistration(ctx context.Context, registrationID, tokenHash string) (MachineRecord, error) {
	var machine MachineRecord
	err := scanMachine(s.db.QueryRowContext(ctx, machineSelect+" WHERE registration_id = ? AND token_hash = ? AND revoked_at IS NULL", registrationID, tokenHash), &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRecord{}, ErrMachineAuthentication
	}
	if err != nil {
		return MachineRecord{}, fmt.Errorf("recover machine registration: %w", err)
	}
	return machine, nil
}

func (s *Store) AuthenticateMachine(ctx context.Context, tokenHash string) (MachineRecord, error) {
	var machine MachineRecord
	err := scanMachine(s.db.QueryRowContext(ctx, machineSelect+" WHERE token_hash = ? AND revoked_at IS NULL", tokenHash), &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRecord{}, ErrMachineAuthentication
	}
	if err != nil {
		return MachineRecord{}, fmt.Errorf("authenticate machine: %w", err)
	}
	return machine, nil
}

const machineSelect = `SELECT machine_id, registration_id, token_hash, hostname, display_name, notes, os, architecture,
	cpu_count, memory_bytes, agent_version, authority_epoch, desired_revision, desired_digest,
	received_revision, received_digest, applied_revision, applied_digest, apply_error,
	pending_authority_epoch, pending_authority_nonce, last_authority_nonce, last_heartbeat_at,
	revoked_at, created_at, updated_at, applied_authority_epoch, core_generation, core_instance_id,
	applied_restriction_digest, restriction_applied_at FROM machines`

func (s *Store) GetMachine(ctx context.Context, machineID string) (MachineRecord, error) {
	var machine MachineRecord
	err := scanMachine(s.db.QueryRowContext(ctx, machineSelect+" WHERE machine_id = ?", machineID), &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRecord{}, ErrMachineNotFound
	}
	if err != nil {
		return MachineRecord{}, fmt.Errorf("read machine: %w", err)
	}
	return machine, nil
}

func (s *Store) ListMachines(ctx context.Context) ([]MachineRecord, error) {
	rows, err := s.db.QueryContext(ctx, machineSelect+" ORDER BY created_at, machine_id")
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer rows.Close()
	var machines []MachineRecord
	for rows.Next() {
		var machine MachineRecord
		if err := scanMachine(rows, &machine); err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		machines = append(machines, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	return machines, nil
}

func scanMachine(scanner rowScanner, machine *MachineRecord) error {
	var cpuCount, memoryBytes, desiredRevision, receivedRevision, appliedRevision, coreGeneration int64
	var pendingEpoch, pendingNonce, lastNonce, heartbeat, revoked, restrictionAppliedAt sql.NullString
	err := scanner.Scan(
		&machine.MachineID, &machine.RegistrationID, &machine.TokenHash, &machine.Hostname, &machine.DisplayName, &machine.Notes,
		&machine.OS, &machine.Architecture, &cpuCount, &memoryBytes, &machine.AgentVersion, &machine.AuthorityEpoch,
		&desiredRevision, &machine.DesiredDigest, &receivedRevision, &machine.ReceivedDigest, &appliedRevision, &machine.AppliedDigest,
		&machine.ApplyError, &pendingEpoch, &pendingNonce, &lastNonce, &heartbeat, &revoked, &machine.CreatedAt, &machine.UpdatedAt,
		&machine.AppliedAuthorityEpoch, &coreGeneration, &machine.CoreInstanceID, &machine.AppliedRestrictionDigest, &restrictionAppliedAt,
	)
	if err != nil {
		return err
	}
	if cpuCount < 0 || memoryBytes < 0 || desiredRevision < 1 || receivedRevision < 0 || appliedRevision < 0 || coreGeneration < 0 {
		return errors.New("machine record contains invalid counters")
	}
	machine.CPUCount = int(cpuCount)
	machine.MemoryBytes = uint64(memoryBytes)
	machine.DesiredRevision = uint64(desiredRevision)
	machine.ReceivedRevision = uint64(receivedRevision)
	machine.AppliedRevision = uint64(appliedRevision)
	machine.CoreGeneration = uint64(coreGeneration)
	machine.PendingAuthorityEpoch = pendingEpoch.String
	machine.PendingAuthorityNonce = pendingNonce.String
	machine.LastAuthorityNonce = lastNonce.String
	machine.LastHeartbeatAt = heartbeat.String
	machine.RestrictionAppliedAt = restrictionAppliedAt.String
	machine.Revoked = revoked.Valid
	return nil
}

func (s *Store) RevokeMachine(ctx context.Context, machineID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin machine revocation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE machines SET revoked_at = ?, updated_at = ? WHERE machine_id = ? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), machineID)
	if err != nil {
		return fmt.Errorf("revoke machine: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("verify machine revocation: %w", err)
		}
		return ErrMachineNotFound
	}
	if err := s.refreshDependentMachineDesiredTx(ctx, tx, machineID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateMachineDetails(ctx context.Context, machineID, displayName, notes string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE machines SET display_name = ?, notes = ?, updated_at = ? WHERE machine_id = ?", strings.TrimSpace(displayName), strings.TrimSpace(notes), now.UTC().Format(time.RFC3339Nano), machineID)
	if err != nil {
		return fmt.Errorf("update machine details: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("verify machine update: %w", err)
		}
		return ErrMachineNotFound
	}
	return nil
}

func (s *Store) DesiredStateExchange(ctx context.Context, machineID string, request protocol.DesiredStateRequest) (protocol.DesiredStateResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.DesiredStateResponse{}, fmt.Errorf("begin desired state exchange: %w", err)
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return protocol.DesiredStateResponse{}, err
	}
	if machine.Revoked {
		return protocol.DesiredStateResponse{}, ErrMachineAuthentication
	}
	authority, err := s.machineDesiredTx(ctx, tx, machine)
	if err != nil {
		return protocol.DesiredStateResponse{}, err
	}
	// D08's fixture-only reporting switch must not strand production usage.
	// Upgrade an old, policy-bearing snapshot once, preserving revision/digest
	// semantics rather than changing an authenticated snapshot in place.
	if !authority.Snapshot.TrafficReportingEnabled && len(authority.Snapshot.PeriodPolicies) > 0 {
		if err := s.refreshMachineDesiredTx(ctx, tx, machineID); err != nil {
			return protocol.DesiredStateResponse{}, err
		}
		machine, err = getMachineTx(ctx, tx, machineID)
		if err != nil {
			return protocol.DesiredStateResponse{}, err
		}
		authority, err = s.machineDesiredTx(ctx, tx, machine)
		if err != nil {
			return protocol.DesiredStateResponse{}, err
		}
	}
	response := protocol.DesiredStateResponse{RequestID: request.RequestID, AuthorityEpoch: authority.Epoch, Revision: authority.Revision, Digest: authority.Digest}
	if machine.AuthorityEpoch != authority.Epoch {
		if request.AuthorityEpoch == authority.Epoch && request.AuthorityNonce != "" &&
			request.FullSnapshotReceived &&
			request.AuthorityNonce == machine.PendingAuthorityNonce && machine.PendingAuthorityEpoch == authority.Epoch &&
			request.Revision == authority.Revision && request.Digest == authority.Digest {
			_, err := tx.ExecContext(ctx, `UPDATE machines SET authority_epoch = ?, desired_revision = ?, desired_digest = ?,
				pending_authority_epoch = NULL, pending_authority_nonce = NULL, last_authority_nonce = ?,
				received_revision = ?, received_digest = ?, apply_error = ?, updated_at = ? WHERE machine_id = ?`,
				authority.Epoch, authority.Revision, authority.Digest, request.AuthorityNonce,
				authority.Revision, authority.Digest, protocol.ErrorCoreNotImplemented, time.Now().UTC().Format(time.RFC3339Nano), machineID)
			if err != nil {
				return protocol.DesiredStateResponse{}, fmt.Errorf("confirm authority handoff: %w", err)
			}
			response.Unchanged = true
			response.AcknowledgedNonce = request.AuthorityNonce
			if err := tx.Commit(); err != nil {
				return protocol.DesiredStateResponse{}, fmt.Errorf("commit authority handoff: %w", err)
			}
			return response, nil
		}
		nonce := machine.PendingAuthorityNonce
		if machine.PendingAuthorityEpoch != authority.Epoch || nonce == "" {
			nonce, err = security.NewToken(security.MachineSecretBytes)
			if err != nil {
				return protocol.DesiredStateResponse{}, err
			}
			_, err = tx.ExecContext(ctx, "UPDATE machines SET pending_authority_epoch = ?, pending_authority_nonce = ? WHERE machine_id = ?", authority.Epoch, nonce, machineID)
			if err != nil {
				return protocol.DesiredStateResponse{}, fmt.Errorf("prepare authority handoff: %w", err)
			}
		}
		response.AuthorityChange = true
		response.AuthorityNonce = nonce
		snapshot := authority.Snapshot
		response.DesiredState = &snapshot
		if err := tx.Commit(); err != nil {
			return protocol.DesiredStateResponse{}, fmt.Errorf("commit authority challenge: %w", err)
		}
		return response, nil
	}
	if request.AuthorityEpoch != authority.Epoch {
		return protocol.DesiredStateResponse{}, ErrAuthorityMismatch
	}
	if request.AuthorityNonce != "" {
		if request.AuthorityNonce != machine.LastAuthorityNonce || !request.FullSnapshotReceived {
			return protocol.DesiredStateResponse{}, ErrAuthorityMismatch
		}
		response.AcknowledgedNonce = request.AuthorityNonce
	} else if request.FullSnapshotReceived {
		return protocol.DesiredStateResponse{}, ErrAuthorityMismatch
	}
	if request.Revision == authority.Revision && request.Digest != authority.Digest {
		return protocol.DesiredStateResponse{}, ErrDigestMismatch
	}
	if request.Revision > authority.Revision {
		return protocol.DesiredStateResponse{}, ErrStaleRevision
	}
	if request.Revision == authority.Revision {
		response.Unchanged = true
	} else {
		snapshot := authority.Snapshot
		response.DesiredState = &snapshot
		_, err := tx.ExecContext(ctx, "UPDATE machines SET desired_revision = ?, desired_digest = ? WHERE machine_id = ?", authority.Revision, authority.Digest, machineID)
		if err != nil {
			return protocol.DesiredStateResponse{}, fmt.Errorf("update machine desired revision: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.DesiredStateResponse{}, fmt.Errorf("commit desired state exchange: %w", err)
	}
	return response, nil
}

func getMachineTx(ctx context.Context, tx *sql.Tx, machineID string) (MachineRecord, error) {
	var machine MachineRecord
	if err := scanMachine(tx.QueryRowContext(ctx, machineSelect+" WHERE machine_id = ?", machineID), &machine); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MachineRecord{}, ErrMachineNotFound
		}
		return MachineRecord{}, fmt.Errorf("read machine: %w", err)
	}
	return machine, nil
}

func (s *Store) RecordMachineStatus(ctx context.Context, machineID string, request protocol.StatusRequest, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin machine status update: %w", err)
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return "", err
	}
	if machine.Revoked {
		return "", ErrMachineAuthentication
	}
	authority, err := s.machineDesiredTx(ctx, tx, machine)
	if err != nil {
		return "", err
	}
	if request.AuthorityEpoch != authority.Epoch || request.AuthorityEpoch != machine.AuthorityEpoch {
		return "", ErrAuthorityMismatch
	}
	if request.Revision != authority.Revision {
		return "", ErrStaleRevision
	}
	if request.Digest != authority.Digest {
		return "", ErrDigestMismatch
	}
	desiredRestrictionDigest, err := authority.Snapshot.QuotaRestrictions.Digest()
	if err != nil {
		return "", fmt.Errorf("calculate restriction digest: %w", err)
	}
	if request.AppliedRestrictionDigest != "" && !security.IsValidDigest(request.AppliedRestrictionDigest) {
		return "", ErrDigestMismatch
	}
	if request.RestrictionAppliedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, request.RestrictionAppliedAt); err != nil {
			return "", ErrDigestMismatch
		}
	}
	if request.CoreGeneration == 0 && (request.Applied || request.AppliedRevision != 0 || request.AppliedDigest != "" || request.AppliedAuthorityEpoch != "" || request.CoreInstanceID != "" || request.AppliedRestrictionDigest != "" || request.RestrictionAppliedAt != "") {
		return "", ErrCoreNotImplemented
	}
	if !request.Received {
		return "", ErrDesiredStateNotReceived
	}
	if request.CPUCount < 0 || request.CPUCount > 4096 || request.MemoryBytes > 1<<60 || len(request.Hostname) > 255 || len(request.DisplayName) > 128 || len(request.OS) > 64 || len(request.Architecture) > 64 || len(request.AgentVersion) > 64 {
		return "", errors.New("machine status contains invalid fields")
	}
	if machine.ReceivedRevision > request.Revision {
		return "", ErrStaleRevision
	}
	if machine.ReceivedRevision == request.Revision && machine.ReceivedDigest != "" && machine.ReceivedDigest != request.Digest {
		return "", ErrDigestMismatch
	}
	applyError := request.ApplyError
	if request.CoreGeneration == 0 && applyError == "" {
		applyError = protocol.ErrorCoreNotImplemented
	}
	if !protocol.IsCoreApplyError(applyError) {
		return "", errors.New("unknown apply error code")
	}
	if request.CoreGeneration > 0 {
		if request.CoreGeneration > 1<<63-1 || request.CoreGeneration < machine.CoreGeneration || !security.IsUUIDv4(request.CoreInstanceID) ||
			(request.CoreGeneration == machine.CoreGeneration && request.CoreInstanceID != machine.CoreInstanceID) {
			return "", ErrStaleRevision
		}
		if request.AppliedRevision != 0 {
			if !security.IsUUIDv4(request.AppliedAuthorityEpoch) || !security.IsValidDigest(request.AppliedDigest) || request.AppliedRevision > 1<<63-1 {
				return "", ErrDigestMismatch
			}
		} else if request.AppliedAuthorityEpoch != "" || request.AppliedDigest != "" {
			return "", ErrDigestMismatch
		}
		if request.AppliedRestrictionDigest == "" && request.RestrictionAppliedAt != "" || request.AppliedRestrictionDigest != "" && request.RestrictionAppliedAt == "" {
			return "", ErrDigestMismatch
		}
		if request.Applied && (applyError != "" || request.AppliedAuthorityEpoch != request.AuthorityEpoch || request.AppliedRevision != request.Revision || request.AppliedDigest != request.Digest) {
			return "", ErrDigestMismatch
		}
		if request.Applied && request.AppliedRestrictionDigest != "" && request.AppliedRestrictionDigest != desiredRestrictionDigest {
			return "", ErrDigestMismatch
		}
		machine.AppliedAuthorityEpoch, machine.AppliedRevision, machine.AppliedDigest = request.AppliedAuthorityEpoch, request.AppliedRevision, request.AppliedDigest
		if request.AppliedRestrictionDigest != "" {
			machine.AppliedRestrictionDigest = request.AppliedRestrictionDigest
		}
		if request.RestrictionAppliedAt != "" {
			machine.RestrictionAppliedAt = request.RestrictionAppliedAt
		}
		machine.CoreGeneration, machine.CoreInstanceID = request.CoreGeneration, request.CoreInstanceID
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `UPDATE machines SET hostname = ?, display_name = CASE WHEN ? = '' THEN display_name ELSE ? END,
		os = ?, architecture = ?, cpu_count = ?, memory_bytes = ?, agent_version = ?, received_revision = ?,
		received_digest = ?, apply_error = ?, last_heartbeat_at = ?, updated_at = ?,
		applied_authority_epoch = ?, applied_revision = ?, applied_digest = ?, core_generation = ?, core_instance_id = ?,
		applied_restriction_digest = ?, restriction_applied_at = ? WHERE machine_id = ?`,
		request.Hostname, request.DisplayName, strings.TrimSpace(request.DisplayName), request.OS, request.Architecture,
		request.CPUCount, request.MemoryBytes, request.AgentVersion, request.Revision, request.Digest, applyError,
		nowText, nowText, machine.AppliedAuthorityEpoch, machine.AppliedRevision, machine.AppliedDigest, machine.CoreGeneration, machine.CoreInstanceID,
		machine.AppliedRestrictionDigest, nullableString(machine.RestrictionAppliedAt), machineID)
	if err != nil {
		return "", fmt.Errorf("persist machine status: %w", err)
	}
	if err := s.refreshDependentMachineDesiredTx(ctx, tx, machineID); err != nil {
		return "", fmt.Errorf("refresh dependent machine snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit machine status: %w", err)
	}
	return nowText, nil
}

func MachineViewFromRecord(record MachineRecord) protocol.MachineView {
	return protocol.MachineView{
		MachineID: record.MachineID, Hostname: record.Hostname, DisplayName: record.DisplayName, Notes: record.Notes,
		OS: record.OS, Architecture: record.Architecture, CPUCount: record.CPUCount, MemoryBytes: record.MemoryBytes,
		AgentVersion: record.AgentVersion, AuthorityEpoch: record.AuthorityEpoch, DesiredRevision: record.DesiredRevision,
		ReceivedRevision: record.ReceivedRevision, ReceivedDigest: record.ReceivedDigest, AppliedRevision: record.AppliedRevision,
		AppliedDigest: record.AppliedDigest, ApplyError: record.ApplyError, LastHeartbeat: record.LastHeartbeatAt, Revoked: record.Revoked,
		AppliedAuthorityEpoch: record.AppliedAuthorityEpoch, CoreGeneration: record.CoreGeneration, CoreInstanceID: record.CoreInstanceID,
		AppliedRestrictionDigest: record.AppliedRestrictionDigest, RestrictionAppliedAt: record.RestrictionAppliedAt,
	}
}
