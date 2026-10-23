package security

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAdminPasswordHash(t *testing.T) {
	password := []byte("correct horse battery staple")
	hash, err := HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyAdminPassword(hash, password) || VerifyAdminPassword(hash, []byte("wrong password")) {
		t.Fatal("password verification returned an unexpected result")
	}
	if _, err := HashAdminPassword([]byte("short")); !errors.Is(err, ErrInvalidAdminPassword) {
		t.Fatalf("short password error = %v", err)
	}
	if _, err := HashAdminPassword(make([]byte, 73)); !errors.Is(err, ErrInvalidAdminPassword) {
		t.Fatalf("long password error = %v", err)
	}
}

func TestMasterKeyCreatedOnceAndMissingKeyDoesNotMutateDatabase(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(dir, "panel.db")
	first, err := LoadOrCreateMasterKey(dir, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateMasterKey(dir, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != MasterKeySize || string(first) != string(second) {
		t.Fatal("master key was not stable across loads")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, MasterKeyFileName))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("master key permissions = %o, want 600", info.Mode().Perm())
		}
	}

	if err := os.Remove(filepath.Join(dir, MasterKeyFileName)); err != nil {
		t.Fatal(err)
	}
	original := []byte("existing encrypted database bytes")
	if err := os.WriteFile(databasePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMasterKey(dir, databasePath); !errors.Is(err, ErrDatabaseExistsWithoutKey) {
		t.Fatalf("missing-key error = %v", err)
	}
	got, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("database contents changed after missing-key refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("master key unexpectedly recreated: %v", err)
	}
}
