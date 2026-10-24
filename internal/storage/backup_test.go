package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRemainsReadableWhileSettingsChange(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < 64; i++ {
			if err := store.SaveSettings(ctx, Settings{SiteName: fmt.Sprintf("concurrent-%d", i)}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	for i := 0; i < 8; i++ {
		snapshot, err := store.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "snapshot.db")
		if err := os.WriteFile(path, snapshot.Database, 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, err := inspectSnapshot(ctx, path)
		if err != nil {
			t.Fatalf("snapshot %d is not a readable generation: %v", i, err)
		}
		if metadata.InstallationID != snapshot.InstallationID || metadata.SchemaVersion != snapshot.SchemaVersion {
			t.Fatalf("snapshot metadata drifted: %+v versus %+v", metadata, snapshot)
		}
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("concurrent settings writer failed: %v", err)
	}
}
