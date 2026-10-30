// Package ledger persists source watermarks and cumulative segments in one
// SQLite transaction. It does not implement Panel ingest or quota enforcement.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

var ErrLedger = errors.New("账本状态或报告确认不一致，保留数据等待核对")

type Metadata struct {
	MachineID      string                  `json:"machine_id"`
	Epoch          string                  `json:"ledger_epoch"`
	Allocated      uint64                  `json:"allocated_close_seq,string"`
	Created        uint64                  `json:"created_segments,string"`
	HighWater      time.Time               `json:"high_water"`
	InstallationID string                  `json:"installation_id,omitempty"`
	Authority      string                  `json:"authority_epoch,omitempty"`
	Receipt        *security.ClosedReceipt `json:"receipt,omitempty"`
}
type identity struct {
	MachineID   string `json:"machine_id"`
	Epoch       string `json:"ledger_epoch"`
	Initialized bool   `json:"initialized"`
}
type Store struct {
	mu             sync.Mutex
	db             *sql.DB
	path           string
	readyAuthority string
	pending        *protocol.TrafficRequest
	recovery       *protocol.TrafficRecoveryRequest
	// Fault hooks are used only by subprocess crash/failed-write tests.
	beforeCommit func() error
	afterCommit  func()
}

const schema = `
CREATE TABLE IF NOT EXISTS ledger_meta(id INTEGER PRIMARY KEY CHECK(id=1),body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY,generation INTEGER NOT NULL,known_zero INTEGER NOT NULL,started TEXT NOT NULL,closed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS streams(id TEXT PRIMARY KEY,binding TEXT NOT NULL UNIQUE,active TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS segments(id TEXT PRIMARY KEY,stream_id TEXT NOT NULL,close_seq INTEGER UNIQUE,body TEXT NOT NULL,ack_up INTEGER NOT NULL DEFAULT 0,ack_down INTEGER NOT NULL DEFAULT 0,ack_final INTEGER NOT NULL DEFAULT 0,sent_final INTEGER NOT NULL DEFAULT 0,retry_after TEXT NOT NULL DEFAULT '',fault TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS watermarks(source TEXT NOT NULL,label TEXT NOT NULL,stream_id TEXT NOT NULL,up INTEGER NOT NULL,down INTEGER NOT NULL,observed TEXT NOT NULL,binding TEXT NOT NULL,PRIMARY KEY(source,label));
CREATE TABLE IF NOT EXISTS imports(id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS policies(id TEXT PRIMARY KEY,scope_key TEXT NOT NULL,effective_at TEXT NOT NULL,body TEXT NOT NULL,UNIQUE(scope_key,effective_at));
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY,code TEXT NOT NULL,start TEXT NOT NULL,end TEXT NOT NULL);
PRAGMA user_version=2;
`

func Open(dataDir, machine string) (*Store, error) {
	if !security.IsUUIDv4(machine) {
		return nil, ErrLedger
	}
	dir, err := storage.PrepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	path, marker := filepath.Join(dir, "agent.ledger.sqlite"), filepath.Join(dir, "ledger.identity.json")
	for _, p := range []string{path, path + "-wal", path + "-shm", marker} {
		if info, err := os.Lstat(p); err == nil {
			if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
				return nil, ErrLedger
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	var ident identity
	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, ErrLedger
		}
		epoch, err := security.NewUUID()
		if err != nil {
			return nil, err
		}
		ident = identity{MachineID: machine, Epoch: epoch}
		data, _ = json.Marshal(ident)
		if err := storage.WritePrivateAtomic(marker, data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if json.Unmarshal(data, &ident) != nil || ident.MachineID != machine || !security.IsUUIDv4(ident.Epoch) {
		return nil, ErrLedger
	}
	if ident.Initialized {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("ledger database missing; refusing to invent a new epoch: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: uriPath}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA cache_size=-1024", "PRAGMA wal_autocheckpoint=128", "PRAGMA journal_size_limit=1048576"} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	var journal string
	var synchronous int
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		return nil, err
	}
	if err = db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		return nil, err
	}
	if journal != "wal" || synchronous != 2 {
		return nil, errors.New("Agent ledger requires SQLite WAL and synchronous FULL")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > 2 {
		return nil, ErrLedger
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return nil, err
	}
	var existing string
	err = tx.QueryRow("SELECT body FROM ledger_meta WHERE id=1").Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		if ident.Initialized {
			return nil, ErrLedger
		}
		body, _ := json.Marshal(Metadata{MachineID: machine, Epoch: ident.Epoch})
		_, err = tx.Exec("INSERT INTO ledger_meta(id,body) VALUES(1,?)", string(body))
	} else if err == nil {
		var meta Metadata
		if json.Unmarshal([]byte(existing), &meta) != nil || meta.Epoch != ident.Epoch || meta.MachineID != machine {
			return nil, ErrLedger
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	ident.Initialized = true
	data, _ = json.Marshal(ident)
	if err = storage.WritePrivateAtomic(marker, data); err != nil {
		return nil, err
	}
	ok = true
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.db.Close() }
func readMeta(tx *sql.Tx) (Metadata, error) {
	var m Metadata
	var b string
	err := tx.QueryRow("SELECT body FROM ledger_meta WHERE id=1").Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &m)
	}
	if err == nil && m.Created >= 1<<48 {
		err = ErrLedger
	}
	return m, err
}
func writeMeta(tx *sql.Tx, m Metadata) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE ledger_meta SET body=? WHERE id=1", string(b))
	return err
}
func (s *Store) commit(tx *sql.Tx) error {
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}
func textTime(t time.Time) string           { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
func event(tx *sql.Tx, code string, start, end time.Time) error {
	if _, err := tx.Exec("INSERT INTO events(code,start,end) VALUES(?,?,?)", code, textTime(start), textTime(end)); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT 128)")
	return err
}

// RegisterSource is called before launching a new owned Core. Unknown existing
// instances use knownZero=false, establishing a baseline without rebilling history.
func (s *Store) RegisterSource(ctx context.Context, id string, generation uint64, started time.Time, knownZero bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !security.IsUUIDv4(id) || generation == 0 || generation > 1<<63-1 {
		return ErrLedger
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldGeneration uint64
	var oldKnown bool
	err = tx.QueryRowContext(ctx, "SELECT generation,known_zero FROM sources WHERE id=?", id).Scan(&oldGeneration, &oldKnown)
	if err == nil {
		if oldGeneration != generation || oldKnown != knownZero {
			return ErrLedger
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sources(id,generation,known_zero,started) VALUES(?,?,?,?)", id, generation, knownZero, textTime(started)); err != nil {
		return err
	}
	return s.commit(tx)
}

type Diagnostic struct {
	Metadata       Metadata                  `json:"metadata"`
	Pending        int                       `json:"pending_segments"`
	Finalized      int                       `json:"finalized_segments"`
	Sources        int                       `json:"sources"`
	DatabaseBytes  int64                     `json:"database_bytes"`
	Events         []Event                   `json:"events"`
	AvailableBytes *uint64                   `json:"available_bytes,omitempty"`
	LowDisk        bool                      `json:"low_disk"`
	PendingPreview []protocol.TrafficSegment `json:"pending_preview"`
}
type Event struct {
	Code  string `json:"code"`
	Start string `json:"start"`
	End   string `json:"end"`
}

func (s *Store) Diagnostic(ctx context.Context) (Diagnostic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := Diagnostic{Events: []Event{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	d.Metadata, err = readMeta(tx)
	if err != nil {
		return d, err
	}
	for _, q := range []struct {
		sql  string
		dest *int
	}{{"SELECT COUNT(*) FROM segments", &d.Pending}, {"SELECT COUNT(*) FROM segments WHERE close_seq IS NOT NULL", &d.Finalized}, {"SELECT COUNT(*) FROM sources", &d.Sources}} {
		if err = tx.QueryRow(q.sql).Scan(q.dest); err != nil {
			return d, err
		}
	}
	rows, err := tx.Query("SELECT code,start,end FROM events ORDER BY id DESC LIMIT 128")
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Code, &e.Start, &e.End); err != nil {
			return d, err
		}
		d.Events = append(d.Events, e)
	}
	if err = rows.Err(); err != nil {
		return d, err
	}
	rows.Close()
	d.PendingPreview = []protocol.TrafficSegment{}
	preview, err := tx.Query("SELECT body FROM segments ORDER BY CASE WHEN close_seq IS NULL THEN 1 ELSE 0 END,close_seq,id LIMIT 32")
	if err != nil {
		return d, err
	}
	defer preview.Close()
	for preview.Next() {
		var b string
		var seg protocol.TrafficSegment
		if err = preview.Scan(&b); err != nil {
			return d, err
		}
		if err = json.Unmarshal([]byte(b), &seg); err != nil {
			return d, err
		}
		d.PendingPreview = append(d.PendingPreview, seg)
	}
	if err = preview.Err(); err != nil {
		return d, err
	}
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if f, err := os.Stat(p); err == nil {
			d.DatabaseBytes += f.Size()
		}
	}
	if available, err := availableBytes(filepath.Dir(s.path)); err == nil {
		d.AvailableBytes = &available
		d.LowDisk = available < 16<<20
	}
	return d, rows.Err()
}

// Inspect never initializes/rebuilds a ledger and can run while the Agent owns
// its normal process lock. SQLite supplies a coherent read transaction.
func Inspect(ctx context.Context, dataDir string) (Diagnostic, error) {
	path, err := filepath.Abs(filepath.Join(dataDir, "agent.ledger.sqlite"))
	if err != nil {
		return Diagnostic{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Diagnostic{}, err
	}
	if !info.Mode().IsRegular() {
		return Diagnostic{}, ErrLedger
	}
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String())
	if err != nil {
		return Diagnostic{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return Diagnostic{}, err
	}
	if version < 1 || version > 2 {
		return Diagnostic{}, ErrLedger
	}
	s := Store{db: db, path: path}
	return s.Diagnostic(ctx)
}
