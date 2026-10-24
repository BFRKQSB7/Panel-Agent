package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

var (
	ErrUpgradeIntentInvalid  = errors.New("upgrade intent is invalid")
	ErrUpgradeIntentNotFound = errors.New("upgrade intent not found")
	ErrUpgradeIntentStale    = errors.New("upgrade intent authority is stale")
	ErrUpgradeIntentTerminal = errors.New("upgrade intent is already terminal")
	upgradeVersionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

type UpgradeIntentInput struct {
	MachineID     string
	TargetVersion string
	Source        string
	SHA256        string
	PackageSize   uint64
}

func (s *Store) CreateUpgradeIntent(ctx context.Context, input UpgradeIntentInput, now time.Time) (protocol.UpgradeIntent, error) {
	if err := validateUpgradeIntentInput(input); err != nil {
		return protocol.UpgradeIntent{}, err
	}
	machineID := strings.ToLower(strings.TrimSpace(input.MachineID))
	if !security.IsUUIDv4(machineID) {
		return protocol.UpgradeIntent{}, ErrUpgradeIntentInvalid
	}
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.UpgradeIntent{}, err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return protocol.UpgradeIntent{}, err
	}
	if machine.Revoked {
		return protocol.UpgradeIntent{}, ErrMachineAuthentication
	}
	authority, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return protocol.UpgradeIntent{}, err
	}
	if machine.AuthorityEpoch != authority.Epoch {
		return protocol.UpgradeIntent{}, ErrUpgradeIntentStale
	}
	id, err := security.NewUUID()
	if err != nil {
		return protocol.UpgradeIntent{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upgrade_intents
		(id,machine_id,authority_epoch,target_version,source,sha256,package_size,state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,'pending',?,?)`, id, machineID, authority.Epoch, input.TargetVersion, input.Source, strings.ToLower(input.SHA256), int64(input.PackageSize), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return protocol.UpgradeIntent{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.UpgradeIntent{}, err
	}
	return s.GetUpgradeIntent(ctx, id)
}

func (s *Store) GetUpgradeIntent(ctx context.Context, id string) (protocol.UpgradeIntent, error) {
	if !security.IsUUIDv4(id) {
		return protocol.UpgradeIntent{}, ErrUpgradeIntentInvalid
	}
	intent, err := scanUpgradeIntent(s.db.QueryRowContext(ctx, upgradeIntentSelect+" WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.UpgradeIntent{}, ErrUpgradeIntentNotFound
	}
	if err != nil {
		return protocol.UpgradeIntent{}, err
	}
	return intent, nil
}

func (s *Store) ListUpgradeIntents(ctx context.Context, machineID string) ([]protocol.UpgradeIntent, error) {
	if !security.IsUUIDv4(machineID) {
		return nil, ErrUpgradeIntentInvalid
	}
	rows, err := s.db.QueryContext(ctx, upgradeIntentSelect+" WHERE machine_id=? ORDER BY created_at DESC, id DESC", machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []protocol.UpgradeIntent{}
	for rows.Next() {
		intent, err := scanUpgradeIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	return result, rows.Err()
}

// PullUpgradeIntents atomically changes pending work to running. A stale
// authority cannot discover or resurrect a historical intent.
func (s *Store) PullUpgradeIntents(ctx context.Context, machineID, authorityEpoch string, now time.Time) ([]protocol.UpgradeIntent, error) {
	if !security.IsUUIDv4(machineID) || !security.IsUUIDv4(authorityEpoch) {
		return nil, ErrUpgradeIntentInvalid
	}
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return nil, err
	}
	authority, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return nil, err
	}
	if machine.Revoked {
		return nil, ErrMachineAuthentication
	}
	if machine.AuthorityEpoch != authority.Epoch || authority.Epoch != authorityEpoch {
		return nil, ErrUpgradeIntentStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE upgrade_intents SET state='running',started_at=COALESCE(started_at,?),updated_at=?
		WHERE machine_id=? AND authority_epoch=? AND state='pending'`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), machineID, authorityEpoch); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, upgradeIntentSelect+" WHERE machine_id=? AND authority_epoch=? AND state='running' ORDER BY created_at, id", machineID, authorityEpoch)
	if err != nil {
		return nil, err
	}
	result := []protocol.UpgradeIntent{}
	for rows.Next() {
		intent, scanErr := scanUpgradeIntent(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		result = append(result, intent)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) RecordUpgradeResult(ctx context.Context, machineID string, request protocol.UpgradeResultRequest, now time.Time) error {
	if !security.IsUUIDv4(machineID) || !security.IsUUIDv4(request.IntentID) || !security.IsUUIDv4(request.AuthorityEpoch) || !upgradeResultState(request.State) || !validUpgradeVersion(request.TargetVersion) || len([]byte(request.TargetVersion)) > protocol.MaxUpgradeVersionBytes || len([]byte(request.ResultCode)) > protocol.MaxUpgradeDetailBytes || len([]byte(request.ResultDetail)) > protocol.MaxUpgradeDetailBytes || len([]byte(request.ActualVersion)) > protocol.MaxUpgradeVersionBytes || request.CoreGeneration > 1<<63-1 {
		return ErrUpgradeIntentInvalid
	}
	now = trafficNow(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	machine, err := getMachineTx(ctx, tx, machineID)
	if err != nil {
		return err
	}
	authority, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return err
	}
	if machine.Revoked {
		return ErrMachineAuthentication
	}
	if machine.AuthorityEpoch != authority.Epoch || authority.Epoch != request.AuthorityEpoch {
		return ErrUpgradeIntentStale
	}
	current, err := scanUpgradeIntent(tx.QueryRowContext(ctx, upgradeIntentSelect+" WHERE id=? AND machine_id=?", request.IntentID, machineID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUpgradeIntentNotFound
		}
		return err
	}
	if current.AuthorityEpoch != request.AuthorityEpoch || current.TargetVersion != request.TargetVersion {
		return ErrUpgradeIntentStale
	}
	if current.State == protocol.UpgradeStateSucceeded || current.State == protocol.UpgradeStateFailed || current.State == protocol.UpgradeStateCanceled {
		if current.State == request.State && current.ResultCode == request.ResultCode && current.ActualVersion == request.ActualVersion && current.RolledBack == request.RolledBack {
			return tx.Commit()
		}
		return ErrUpgradeIntentTerminal
	}
	completed := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE upgrade_intents SET state=?,result_code=?,result_detail=?,actual_version=?,rolled_back=?,core_generation=?,updated_at=?,completed_at=? WHERE id=? AND machine_id=? AND state IN ('pending','running')`, request.State, strings.TrimSpace(request.ResultCode), strings.TrimSpace(request.ResultDetail), strings.TrimSpace(request.ActualVersion), boolInt(request.RolledBack), int64(request.CoreGeneration), completed, completed, request.IntentID, machineID); err != nil {
		return err
	}
	return tx.Commit()
}

func validateUpgradeIntentInput(input UpgradeIntentInput) error {
	if !security.IsUUIDv4(strings.ToLower(strings.TrimSpace(input.MachineID))) || !validUpgradeVersion(input.TargetVersion) || len([]byte(input.TargetVersion)) > protocol.MaxUpgradeVersionBytes || input.Source != protocol.UpgradeSourceOfficialXray || !security.IsValidDigest(strings.ToLower(input.SHA256)) || input.PackageSize == 0 || input.PackageSize > protocol.MaxUpgradePackageBytes {
		return ErrUpgradeIntentInvalid
	}
	return nil
}

func validUpgradeVersion(value string) bool {
	if !upgradeVersionPattern.MatchString(value) {
		return false
	}
	for _, component := range strings.Split(value, ".") {
		if len(component) > 1 && component[0] == '0' {
			return false
		}
		if _, err := strconv.ParseUint(component, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func upgradeResultState(value string) bool {
	return value == protocol.UpgradeStateSucceeded || value == protocol.UpgradeStateFailed
}

const upgradeIntentSelect = `SELECT id,machine_id,authority_epoch,target_version,source,sha256,package_size,state,result_code,result_detail,actual_version,rolled_back,core_generation,created_at,updated_at,started_at,completed_at FROM upgrade_intents`

func scanUpgradeIntent(scanner rowScanner) (protocol.UpgradeIntent, error) {
	var intent protocol.UpgradeIntent
	var packageSize, generation int64
	var rolledBack int
	var started, completed sql.NullString
	if err := scanner.Scan(&intent.ID, &intent.MachineID, &intent.AuthorityEpoch, &intent.TargetVersion, &intent.Source, &intent.SHA256, &packageSize, &intent.State, &intent.ResultCode, &intent.ResultDetail, &intent.ActualVersion, &rolledBack, &generation, &intent.CreatedAt, &intent.UpdatedAt, &started, &completed); err != nil {
		return protocol.UpgradeIntent{}, err
	}
	if packageSize <= 0 || generation < 0 || (rolledBack != 0 && rolledBack != 1) || !security.IsUUIDv4(intent.ID) || !security.IsUUIDv4(intent.MachineID) || !security.IsUUIDv4(intent.AuthorityEpoch) || !validUpgradeVersion(intent.TargetVersion) || intent.Source != protocol.UpgradeSourceOfficialXray || !security.IsValidDigest(intent.SHA256) || !upgradeResultState(intent.State) && intent.State != protocol.UpgradeStatePending && intent.State != protocol.UpgradeStateRunning && intent.State != protocol.UpgradeStateCanceled || uint64(packageSize) > protocol.MaxUpgradePackageBytes {
		return protocol.UpgradeIntent{}, fmt.Errorf("invalid persisted upgrade intent")
	}
	intent.Version = protocol.UpgradeProtocolVersion
	intent.PackageSize, intent.RolledBack, intent.CoreGeneration = uint64(packageSize), rolledBack == 1, uint64(generation)
	intent.StartedAt, intent.CompletedAt = started.String, completed.String
	return intent, nil
}
