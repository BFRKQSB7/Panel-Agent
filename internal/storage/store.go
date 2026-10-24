package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db        *sql.DB
	masterKey []byte
	dataDir   string
	database  string
	backupMu  sync.Mutex
}

type Settings struct {
	SiteName string
}

func Open(dataDir string) (*Store, error) {
	databasePath, err := filepath.Abs(filepath.Join(dataDir, DatabaseFileName))
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite database path: %w", err)
	}
	masterKey, err := security.LoadOrCreateMasterKey(dataDir, databasePath)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(databasePath)
	if runtime.GOOS == "windows" && len(uriPath) >= 2 && uriPath[1] == ':' {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{
		Scheme: "file",
		Path:   uriPath,
		RawQuery: strings.Join([]string{
			"_busy_timeout=5000",
			"_foreign_keys=1",
			"_journal_mode=WAL",
			"_synchronous=FULL",
		}, "&"),
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to SQLite database: %w", err)
	}
	if err := applyMigrations(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db, masterKey: masterKey, dataDir: filepath.Dir(databasePath), database: databasePath}
	if err := store.ensurePanelAuthority(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.ensureMachinePeriodSnapshots(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("list schema migrations: %w", err)
	}
	type migrationFile struct {
		version int
		name    string
	}
	var migrations []migrationFile
	versions := make(map[int]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if _, exists := versions[version]; exists {
			return fmt.Errorf("duplicate schema migration version %d", version)
		}
		versions[version] = struct{}{}
		migrations = append(migrations, migrationFile{version: version, name: entry.Name()})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	if len(migrations) == 0 {
		return errors.New("no schema migrations were embedded")
	}
	if current > migrations[len(migrations)-1].version {
		return fmt.Errorf("database schema version %d is newer than this Panel supports", current)
	}
	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		if migration.version != current+1 {
			return fmt.Errorf("schema migration %d is missing", current+1)
		}
		contents, err := migrationFiles.ReadFile(filepath.ToSlash(filepath.Join("migrations", migration.name)))
		if err != nil {
			return fmt.Errorf("read schema migration %d: %w", migration.version, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", migration.version, err)
		}
		for _, statement := range splitSQLStatements(string(contents)) {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)", migration.version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", migration.version, err)
		}
		current = migration.version
	}
	return nil
}

func (s *Store) ensurePanelAuthority(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM panel_authority WHERE id = 1").Scan(&count); err != nil {
		return fmt.Errorf("read Panel authority: %w", err)
	}
	if count == 1 {
		return nil
	}
	state := protocol.EmptyDesiredState()
	digest, err := protocol.DigestDesiredState(state)
	if err != nil {
		return fmt.Errorf("digest initial desired state: %w", err)
	}
	authority, err := security.NewUUID()
	if err != nil {
		return err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode initial desired state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO panel_authority(id, authority_epoch, revision, digest, snapshot_json, updated_at) VALUES(1, ?, 1, ?, ?, ?)", authority, digest, string(stateJSON), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("persist Panel authority: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	clear(s.masterKey)
	return err
}

func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_user WHERE id = 1").Scan(&count); err != nil {
		return false, fmt.Errorf("read administrator state: %w", err)
	}
	return count == 1, nil
}

func (s *Store) InitializeAdmin(ctx context.Context, passwordHash string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate installation identity: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	installationID := hex.EncodeToString(bytes[0:4]) + "-" + hex.EncodeToString(bytes[4:6]) + "-" + hex.EncodeToString(bytes[6:8]) + "-" + hex.EncodeToString(bytes[8:10]) + "-" + hex.EncodeToString(bytes[10:16])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin administrator initialization: %w", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_user").Scan(&exists); err != nil {
		return "", fmt.Errorf("check administrator state: %w", err)
	}
	if exists != 0 {
		return "", errors.New("administrator is already initialized")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO admin_user(id, password_hash, created_at) VALUES(1, ?, ?)", passwordHash, now); err != nil {
		return "", fmt.Errorf("persist administrator: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES('site_name', 'Panel-Agent')"); err != nil {
		return "", fmt.Errorf("persist default settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO installation(id, installation_id, created_at) VALUES(1, ?, ?)", installationID, now); err != nil {
		return "", fmt.Errorf("persist installation identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit administrator initialization: %w", err)
	}
	return installationID, nil
}

func (s *Store) VerifyAdmin(ctx context.Context, verify func(string) bool) (bool, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM admin_user WHERE id = 1").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read administrator credentials: %w", err)
	}
	return verify(hash), nil
}

func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var settings Settings
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'site_name'").Scan(&settings.SiteName); err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	return settings, nil
}

func (s *Store) SaveSettings(ctx context.Context, settings Settings) error {
	result, err := s.db.ExecContext(ctx, "UPDATE settings SET value = ? WHERE key = 'site_name'", settings.SiteName)
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("check saved settings: %w", err)
		}
		return errors.New("settings row is missing")
	}
	return nil
}

func (s *Store) InstallationID(ctx context.Context) (string, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT installation_id FROM installation WHERE id = 1").Scan(&id); err != nil {
		return "", fmt.Errorf("read installation identity: %w", err)
	}
	return id, nil
}
