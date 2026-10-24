package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite"
	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

type BackupSnapshot struct {
	Database       []byte
	MasterKey      []byte
	SchemaVersion  int
	InstallationID string
	Extensions     map[string][]byte
}

// Snapshot uses SQLite's consistent VACUUM INTO operation instead of copying
// panel.db, panel.db-wal and panel.db-shm independently. Extension resources
// are read back from that same snapshot database, so their metadata cannot
// drift to a later commit.
func (s *Store) Snapshot(ctx context.Context) (BackupSnapshot, error) {
	if s == nil || s.db == nil || s.dataDir == "" || len(s.masterKey) != security.MasterKeySize {
		return BackupSnapshot{}, errors.New("Panel store is not ready for backup")
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	token, err := security.NewToken(16)
	if err != nil {
		return BackupSnapshot{}, err
	}
	temporary := filepath.Join(s.dataDir, ".panel-backup-"+token+".db")
	if _, err := os.Lstat(temporary); err == nil {
		return BackupSnapshot{}, errors.New("backup temporary path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupSnapshot{}, err
	}
	defer os.Remove(temporary)
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", temporary); err != nil {
		return BackupSnapshot{}, fmt.Errorf("create consistent SQLite snapshot: %w", err)
	}
	database, err := os.ReadFile(temporary)
	if err != nil {
		return BackupSnapshot{}, fmt.Errorf("read SQLite snapshot: %w", err)
	}
	metadata, err := inspectSnapshot(ctx, temporary)
	if err != nil {
		return BackupSnapshot{}, err
	}
	return BackupSnapshot{
		Database:       database,
		MasterKey:      append([]byte(nil), s.masterKey...),
		SchemaVersion:  metadata.SchemaVersion,
		InstallationID: metadata.InstallationID,
		Extensions:     metadata.Extensions,
	}, nil
}

type snapshotMetadata struct {
	SchemaVersion  int
	InstallationID string
	Extensions     map[string][]byte
}

func inspectSnapshot(ctx context.Context, path string) (snapshotMetadata, error) {
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return snapshotMetadata{}, err
	}
	defer db.Close()
	var schema int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&schema); err != nil || schema < 1 {
		return snapshotMetadata{}, fmt.Errorf("read backup schema version: %w", err)
	}
	var installation string
	if err := db.QueryRowContext(ctx, "SELECT installation_id FROM installation WHERE id=1").Scan(&installation); err != nil || !security.IsUUIDv4(installation) {
		return snapshotMetadata{}, fmt.Errorf("read backup installation identity: %w", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT extension_id,version,package_json FROM protocol_extensions ORDER BY extension_id,version")
	if err != nil {
		return snapshotMetadata{}, fmt.Errorf("read backup extensions: %w", err)
	}
	defer rows.Close()
	extensionFiles := map[string][]byte{}
	for rows.Next() {
		var id, version string
		var packageJSON []byte
		if err := rows.Scan(&id, &version, &packageJSON); err != nil {
			return snapshotMetadata{}, err
		}
		if _, err := extensions.ParseJSON(packageJSON); err != nil {
			return snapshotMetadata{}, fmt.Errorf("validate backup extension %s@%s: %w", id, version, err)
		}
		extensionFiles[id+"@"+version] = append([]byte(nil), packageJSON...)
	}
	if err := rows.Err(); err != nil {
		return snapshotMetadata{}, err
	}
	return snapshotMetadata{SchemaVersion: schema, InstallationID: installation, Extensions: extensionFiles}, nil
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(uriPath) >= 2 && uriPath[1] == ':' {
		uriPath = "/" + uriPath
	}
	dsn := "file:" + uriPath + "?mode=ro&_query_only=1&_foreign_keys=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ValidateIntegrity decrypts every persisted secret family that has a public
// reader. It is used on a staged restore before any active files are replaced.
func (s *Store) ValidateIntegrity(ctx context.Context) error {
	if _, err := s.GetSettings(ctx); err != nil {
		return fmt.Errorf("validate settings: %w", err)
	}
	if _, err := s.InstallationID(ctx); err != nil {
		return fmt.Errorf("validate installation identity: %w", err)
	}
	if _, err := s.GetPanelAuthority(ctx); err != nil {
		return fmt.Errorf("validate Panel authority: %w", err)
	}
	if _, err := s.ListExtensions(ctx); err != nil {
		return fmt.Errorf("validate extensions: %w", err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("validate user credentials: %w", err)
	}
	_ = users
	machines, err := s.ListMachines(ctx)
	if err != nil {
		return fmt.Errorf("validate machines: %w", err)
	}
	for _, machine := range machines {
		if _, err := s.GetMachineDesiredState(ctx, machine.MachineID); err != nil {
			return fmt.Errorf("validate desired snapshot for %s: %w", machine.MachineID, err)
		}
		if _, err := s.ListNodes(ctx, machine.MachineID); err != nil {
			return fmt.Errorf("validate nodes for %s: %w", machine.MachineID, err)
		}
		if _, err := s.GetExit(ctx, machine.MachineID); err != nil && !errors.Is(err, ErrExitNotFound) {
			return fmt.Errorf("validate Exit for %s: %w", machine.MachineID, err)
		}
	}
	return nil
}

func (s *Store) DataDir() string {
	if s == nil {
		return ""
	}
	return s.dataDir
}

// FinalizeRestore creates a new authority generation while preserving the
// restored desired snapshots and machine credentials. The next Agent exchange
// therefore performs a nonce-bound full snapshot handshake instead of trusting
// a possibly smaller restored revision.
func (s *Store) FinalizeRestore(ctx context.Context, now time.Time) (PanelAuthority, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PanelAuthority{}, err
	}
	defer tx.Rollback()
	current, err := readPanelAuthority(ctx, tx)
	if err != nil {
		return PanelAuthority{}, err
	}
	epoch, err := security.NewUUID()
	if err != nil {
		return PanelAuthority{}, err
	}
	snapshotJSON, err := json.Marshal(current.Snapshot)
	if err != nil {
		return PanelAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE panel_authority SET authority_epoch=?,revision=1,digest=?,snapshot_json=?,updated_at=? WHERE id=1`, epoch, current.Digest, string(snapshotJSON), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return PanelAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM join_tokens"); err != nil {
		return PanelAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM token_operations"); err != nil {
		return PanelAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE machines SET pending_authority_epoch=NULL,pending_authority_nonce=NULL,last_authority_nonce=NULL,
		received_revision=0,received_digest='',applied_authority_epoch='',applied_revision=0,applied_digest='',
		apply_error=?,core_generation=0,core_instance_id='',applied_restriction_digest='',restriction_applied_at=NULL,updated_at=?`, protocol.ErrorCorePending, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return PanelAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE traffic_ledgers SET reconciliation_pending=1,updated_at=?", now.UTC().Format(time.RFC3339Nano)); err != nil {
		return PanelAuthority{}, err
	}
	if err := tx.Commit(); err != nil {
		return PanelAuthority{}, err
	}
	return s.GetPanelAuthority(ctx)
}
