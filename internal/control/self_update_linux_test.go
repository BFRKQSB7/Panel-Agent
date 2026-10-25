package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/backup"
	"panel-agent.local/panel-agent/internal/storage"
)

// The candidate is a real child process which opens the actual Panel store,
// changes SQLite, and may terminate during migration or after binary cutover.
func TestPanelSelfUpdatePublishesOrRestoresCompleteGeneration(t *testing.T) {
	for _, scenario := range []string{"success", "migration-failure", "health-failure", "crash-binary-swapped", "crash-data-swapped", "crash-committed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "data")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			store, err := storage.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.InitializeAdmin(context.Background(), "test-admin-hash"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.InstallExtension(context.Background(), testExtensionPackage(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			before, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(before.Extensions) != 1 {
				t.Fatal("fixture has no extension resource")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			active := filepath.Join(root, "control")
			old := []byte("#!/bin/sh\nprintf 'Panel-Agent 0.1.0\\n'\n")
			if err := os.WriteFile(active, old, 0o700); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(root, "candidate")
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			script := []byte("#!/bin/sh\nexec " + quote(testBinary) + " -test.run '^TestPanelUpdateCandidateWorker$' -- \"$0\" \"$@\"\n")
			if err := os.WriteFile(candidate, script, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PANEL_AGENT_D15_PANEL_SCENARIO", scenario)
			t.Setenv("PANEL_AGENT_D15_PANEL_ACTIVE", active)
			sum := sha256.Sum256(script)
			result, err := SelfUpdateResult{}, error(nil)
			if strings.HasPrefix(scenario, "crash-") {
				t.Setenv("PANEL_AGENT_D15_UPDATE_ROOT", root)
				command := exec.Command(testBinary, "-test.run=^TestPanelUpdateCrashWorker$")
				output, e := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("update worker did not reach durable phase: %v %s", e, output)
				}
				rolledBack, e := recoverPanelSelfUpdate(dir, active)
				if e != nil || rolledBack == (scenario == "crash-committed") {
					t.Fatal("incorrect update recovery decision", rolledBack, e)
				}
			} else {
				result, err = UpdateSelf(context.Background(), SelfUpdateOptions{DataDir: dir, ExecutablePath: active, PackagePath: candidate, PackageSHA256: hex.EncodeToString(sum[:]), TargetVersion: "0.2.0", BackupPassword: "d15-backup-password"})
			}
			keptCandidate := scenario == "success" || scenario == "crash-committed"
			if keptCandidate && err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(scenario, "crash-") && !keptCandidate && err == nil {
				t.Fatal("injected failure reported success")
			}
			actual, readErr := os.ReadFile(active)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if keptCandidate && !bytes.Equal(actual, script) || !keptCandidate && !bytes.Equal(actual, old) {
				t.Fatal("wrong executable generation")
			}
			key, readErr := os.ReadFile(filepath.Join(dir, "master.key"))
			if readErr != nil || !bytes.Equal(key, before.MasterKey) {
				t.Fatal("master key was not retained", readErr)
			}
			store, readErr = storage.Open(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			after, readErr := store.Snapshot(context.Background())
			store.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if after.InstallationID != before.InstallationID || len(after.Extensions) != len(before.Extensions) {
				t.Fatal("installation or extension generation changed")
			}
			for name, contents := range before.Extensions {
				if !bytes.Equal(contents, after.Extensions[name]) {
					t.Fatal("extension resource lost", name)
				}
			}
			db, readErr := sql.Open("sqlite", filepath.Join(dir, "panel.db"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var migrated int
			readErr = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='d15_candidate_migration'").Scan(&migrated)
			db.Close()
			if readErr != nil || keptCandidate != (migrated == 1) {
				t.Fatal("wrong migrated database generation", migrated, readErr)
			}
			backups, readErr := filepath.Glob(filepath.Join(dir, "backups", "panel-update-*.json"))
			if readErr != nil || len(backups) != 1 {
				t.Fatal("missing complete backup", readErr)
			}
			encoded, readErr := os.ReadFile(backups[0])
			if readErr != nil {
				t.Fatal(readErr)
			}
			bundle, readErr := backup.Open(encoded, "d15-backup-password")
			if readErr != nil || bundle.Manifest.InstallationID != before.InstallationID || !bytes.Equal(bundle.MasterKey, before.MasterKey) {
				t.Fatal("pre-update backup is not recoverable", readErr)
			}
			if scenario == "success" && result.BackupPath != backups[0] {
				t.Fatal("wrong backup path")
			}
			t.Logf("scenario=%s executable=ok database=ok key=ok backup=verified", scenario)
		})
	}
}

func TestPanelUpdateCrashWorker(t *testing.T) {
	root := os.Getenv("PANEL_AGENT_D15_UPDATE_ROOT")
	if root == "" {
		t.Skip("subprocess only")
	}
	candidate := filepath.Join(root, "candidate")
	contents, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	_, err = updateSelf(context.Background(), SelfUpdateOptions{DataDir: filepath.Join(root, "data"), ExecutablePath: filepath.Join(root, "control"), PackagePath: candidate, PackageSHA256: hex.EncodeToString(sum[:]), TargetVersion: "0.2.0", BackupPassword: "d15-backup-password"}, func(phase string) {
		if "crash-"+phase == os.Getenv("PANEL_AGENT_D15_PANEL_SCENARIO") {
			os.Exit(77)
		}
	})
	t.Fatal("update crash checkpoint not reached", err)
}

func TestPanelUpdateCandidateWorker(t *testing.T) {
	scenario := os.Getenv("PANEL_AGENT_D15_PANEL_SCENARIO")
	if scenario == "" {
		t.Skip("subprocess only")
	}
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 2 {
		t.Fatal("missing candidate invocation")
	}
	if args[1] == "version" {
		if scenario == "health-failure" && args[0] == os.Getenv("PANEL_AGENT_D15_PANEL_ACTIVE") {
			os.Exit(1)
		}
		fmt.Println("Panel-Agent 0.2.0")
		os.Exit(0)
	}
	if len(args) != 4 || args[1] != "check" || args[2] != "--data-dir" {
		t.Fatal("unexpected candidate arguments")
	}
	server, err := NewServer(Config{DataDir: args[3], Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(args[3], "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE d15_candidate_migration(id INTEGER PRIMARY KEY)")
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if scenario == "migration-failure" {
		os.Exit(1)
	}
	os.Exit(0)
}
