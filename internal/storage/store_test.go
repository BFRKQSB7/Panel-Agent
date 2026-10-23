package storage

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestInitializeAdminPersistsSettingsAndImmutableInstallationID(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := store.InitializeAdmin(ctx, "test-password-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("installation id is not a UUID v4: %q", id)
	}
	if _, err := store.InitializeAdmin(ctx, "another-hash"); err == nil {
		t.Fatal("second administrator initialization unexpectedly succeeded")
	}
	settings, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SiteName != "Panel-Agent" {
		t.Fatalf("default site name = %q", settings.SiteName)
	}
	if err := store.SaveSettings(ctx, Settings{SiteName: "Persisted Panel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE installation SET installation_id = 'changed'"); err == nil {
		t.Fatal("installation identity update unexpectedly succeeded")
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM installation"); err == nil {
		t.Fatal("installation identity deletion unexpectedly succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotID, err := reopened.InstallationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id {
		t.Fatalf("installation id changed across restart: %q != %q", gotID, id)
	}
	settings, err = reopened.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.SiteName != "Persisted Panel" {
		t.Fatalf("saved site name was not persisted: %q", settings.SiteName)
	}
}

func TestMigrationSplitsTriggerBodies(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("migrations", "0001_initial.sql"))
	if err != nil {
		t.Fatal(err)
	}
	statements := splitSQLStatements(string(contents))
	if len(statements) != 5 {
		t.Fatalf("migration statement count = %d, want 5", len(statements))
	}
	for i, statement := range statements {
		if statement == "" {
			t.Fatalf("migration statement %d is empty", i)
		}
	}
}
