package backup

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestStageSwapCommitRestoresStateAndRequiresNewAuthority(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SaveSettings(ctx, storage.Settings{SiteName: "backup generation"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	join, candidate, registration := testRestoreSecrets(t)
	now := time.Now().UTC()
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		store.Close()
		t.Fatal(err)
	}
	machine, oldAuthority, err := store.RegisterMachine(ctx, storage.RegisterMachineInput{
		RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate),
		Hostname: "restore-host", OS: "linux", Architecture: "amd64", AgentVersion: protocol.AgentVersion,
	}, now)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	unusedJoin, unusedCandidate, unusedRegistration := testRestoreSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(unusedJoin), "", now.Add(time.Hour), now); err != nil {
		store.Close()
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	encoded, manifest, err := Create(Input{Database: snapshot.Database, MasterKey: snapshot.MasterKey, SchemaVersion: snapshot.SchemaVersion, InstallationID: snapshot.InstallationID, Extensions: snapshot.Extensions, Password: "restore-pass"})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if manifest.InstallationID != snapshot.InstallationID {
		store.Close()
		t.Fatal("backup manifest lost installation identity")
	}
	decoded, err := Open(encoded, "restore-pass")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stage, err := Stage(ctx, dir, decoded, snapshot.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Swap(dir, stage.Path); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.Open(dir)
	if err != nil {
		_ = Rollback(dir)
		t.Fatal(err)
	}
	newAuthority, err := restored.FinalizeRestore(ctx, now.Add(time.Second))
	if err != nil {
		_ = restored.Close()
		_ = Rollback(dir)
		t.Fatal(err)
	}
	if newAuthority.Epoch == oldAuthority.Epoch {
		t.Fatal("restore reused the old authority epoch")
	}
	settings, err := restored.GetSettings(ctx)
	if err != nil || settings.SiteName != "backup generation" {
		t.Fatalf("restored settings = (%+v, %v)", settings, err)
	}
	if _, _, err := restored.RegisterMachine(ctx, storage.RegisterMachineInput{
		RegistrationID: unusedRegistration, JoinTokenHash: security.HashToken(unusedJoin), CandidateHash: security.HashToken(unusedCandidate),
		Hostname: "should-not-register", OS: "linux", Architecture: "amd64", AgentVersion: protocol.AgentVersion,
	}, now); !errors.Is(err, storage.ErrInvalidJoinToken) {
		t.Fatalf("restore retained an unconsumed Join Token: %v", err)
	}
	challenge, err := restored.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{
		RequestID: "11111111-1111-4111-8111-111111111111", AuthorityEpoch: oldAuthority.Epoch, Revision: oldAuthority.Revision, Digest: oldAuthority.Digest,
	})
	if err != nil || !challenge.AuthorityChange || challenge.AuthorityEpoch != newAuthority.Epoch || challenge.AuthorityNonce == "" || challenge.DesiredState == nil {
		t.Fatalf("restore did not force a full authority challenge: (%+v, %v)", challenge, err)
	}
	acknowledged, err := restored.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{
		RequestID: "22222222-2222-4222-8222-222222222222", AuthorityEpoch: newAuthority.Epoch, Revision: challenge.Revision, Digest: challenge.Digest,
		AuthorityNonce: challenge.AuthorityNonce, FullSnapshotReceived: true,
	})
	if err != nil || acknowledged.AcknowledgedNonce != challenge.AuthorityNonce {
		t.Fatalf("restore authority handshake was not accepted: (%+v, %v)", acknowledged, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetPanelAuthority(ctx)
	if err != nil || persisted.Epoch != newAuthority.Epoch {
		t.Fatalf("committed authority = (%+v, %v)", persisted, err)
	}
}

func TestStageRejectsMasterKeyMismatchWithoutTouchingCurrentData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	snapshot.MasterKey[0] ^= 1
	encoded, _, err := Create(Input{Database: snapshot.Database, MasterKey: snapshot.MasterKey, SchemaVersion: snapshot.SchemaVersion, InstallationID: snapshot.InstallationID})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Open(encoded, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, dir, decoded, snapshot.InstallationID); err == nil {
		t.Fatal("master-key mismatch was accepted")
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetSettings(ctx); err != nil {
		t.Fatalf("current Panel data was damaged by rejected restore: %v", err)
	}
}

func TestStageRejectsFutureSchemaWithoutTouchingCurrentData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Create(Input{
		Database: snapshot.Database, MasterKey: snapshot.MasterKey, SchemaVersion: snapshot.SchemaVersion + 1,
		InstallationID: snapshot.InstallationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Open(encoded, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, dir, decoded, snapshot.InstallationID); err == nil {
		t.Fatal("future schema backup was accepted")
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetSettings(ctx); err != nil {
		t.Fatalf("current Panel data was damaged by rejected schema restore: %v", err)
	}
}

func TestRecoverPendingRollsBackUncommittedGeneration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAdmin(ctx, "hash"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SaveSettings(ctx, storage.Settings{SiteName: "new generation"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SaveSettings(ctx, storage.Settings{SiteName: "current generation"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	encoded, _, err := Create(Input{Database: snapshot.Database, MasterKey: snapshot.MasterKey, SchemaVersion: snapshot.SchemaVersion, InstallationID: snapshot.InstallationID})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	decoded, err := Open(encoded, "")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stage, err := Stage(ctx, dir, decoded, snapshot.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Swap(dir, stage.Path); err != nil {
		t.Fatal(err)
	}
	if err := RecoverPending(dir); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	settings, err := reopened.GetSettings(ctx)
	if err != nil || settings.SiteName != "current generation" {
		t.Fatalf("uncommitted restore was not rolled back: (%+v, %v)", settings, err)
	}
}

func testRestoreSecrets(t *testing.T) (string, string, string) {
	t.Helper()
	join, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return join, candidate, registration
}
