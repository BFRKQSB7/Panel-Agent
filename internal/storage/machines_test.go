package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func TestJoinTokenRegistrationRecoveryAndRePair(t *testing.T) {
	store := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	join, candidate, registrationID := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(30*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	input := testRegistration(join, candidate, registrationID)
	machine, authority, err := store.RegisterMachine(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Epoch == "" || machine.MachineID == "" || machine.TokenHash == candidate {
		t.Fatal("registration did not persist a hashed machine credential and authority")
	}
	if recovered, err := store.RecoverRegistration(ctx, registrationID, security.HashToken(candidate)); err != nil || recovered.MachineID != machine.MachineID {
		t.Fatalf("lost-response recovery = (%q, %v)", recovered.MachineID, err)
	}
	if _, err := store.RecoverRegistration(ctx, registrationID, security.HashToken("different candidate")); !errors.Is(err, ErrMachineAuthentication) {
		t.Fatalf("registration ID alone recovered identity: %v", err)
	}
	if _, _, err := store.RegisterMachine(ctx, input, now); !errors.Is(err, ErrRegistrationConflict) {
		t.Fatalf("duplicate registration result = %v", err)
	}
	if machines, err := store.ListMachines(ctx); err != nil || len(machines) != 1 {
		t.Fatalf("machine count after response recovery = %d, %v", len(machines), err)
	}

	secondJoin, secondCandidate, secondRegistration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(secondJoin), "", now.Add(30*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RegisterMachine(ctx, testRegistration(secondJoin, secondCandidate, secondRegistration), now.Add(31*time.Minute)); !errors.Is(err, ErrExpiredJoinToken) {
		t.Fatalf("expired Join Token result = %v", err)
	}
	if _, _, err := store.RegisterMachine(ctx, testRegistration(join, "another-candidate-secret-which-is-long", "550e8400-e29b-41d4-a716-446655440000"), now); !errors.Is(err, ErrUsedJoinToken) {
		t.Fatalf("reused Join Token result = %v", err)
	}

	repairJoin, repairCandidate, repairRegistration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(repairJoin), machine.MachineID, now.Add(30*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	repaired, _, err := store.RegisterMachine(ctx, testRegistrationFor(repairJoin, repairCandidate, repairRegistration, machine.MachineID), now)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.MachineID != machine.MachineID || repaired.RegistrationID == machine.RegistrationID {
		t.Fatal("re-pair created a different machine identity or retained the old registration")
	}
	if _, err := store.AuthenticateMachine(ctx, security.HashToken(candidate)); !errors.Is(err, ErrMachineAuthentication) {
		t.Fatalf("old machine secret remained valid after re-pair: %v", err)
	}
	if got, err := store.AuthenticateMachine(ctx, security.HashToken(repairCandidate)); err != nil || got.MachineID != machine.MachineID {
		t.Fatalf("new machine secret was not accepted: (%q, %v)", got.MachineID, err)
	}
	if err := store.RevokeMachine(ctx, machine.MachineID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateMachine(ctx, security.HashToken(repairCandidate)); !errors.Is(err, ErrMachineAuthentication) {
		t.Fatalf("revoked machine secret remained valid: %v", err)
	}
}

func TestAuthorityHandoffRejectsOldAndConflictingState(t *testing.T) {
	store := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	join, candidate, registrationID := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, oldAuthority, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registrationID), now)
	if err != nil {
		t.Fatal(err)
	}
	newAuthority, err := store.RotatePanelAuthority(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.DesiredStateRequest{RequestID: "550e8400-e29b-41d4-a716-446655440001", AuthorityEpoch: oldAuthority.Epoch, Revision: oldAuthority.Revision, Digest: oldAuthority.Digest}
	challenge, err := store.DesiredStateExchange(ctx, machine.MachineID, request)
	if err != nil || !challenge.AuthorityChange || challenge.AuthorityEpoch != newAuthority.Epoch || challenge.AuthorityNonce == "" || challenge.DesiredState == nil {
		t.Fatalf("authority challenge = (%+v, %v)", challenge, err)
	}

	latestAuthority, err := store.RotatePanelAuthority(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	staleAcknowledgement := protocol.DesiredStateRequest{RequestID: "550e8400-e29b-41d4-a716-446655440002", AuthorityEpoch: newAuthority.Epoch, Revision: newAuthority.Revision, Digest: newAuthority.Digest, AuthorityNonce: challenge.AuthorityNonce, FullSnapshotReceived: true}
	secondChallenge, err := store.DesiredStateExchange(ctx, machine.MachineID, staleAcknowledgement)
	if err != nil || !secondChallenge.AuthorityChange || secondChallenge.AuthorityEpoch != latestAuthority.Epoch || secondChallenge.AuthorityNonce == challenge.AuthorityNonce {
		t.Fatalf("stale handoff was not replaced by the current challenge: (%+v, %v)", secondChallenge, err)
	}

	acknowledgement := protocol.DesiredStateRequest{RequestID: "550e8400-e29b-41d4-a716-446655440003", AuthorityEpoch: latestAuthority.Epoch, Revision: latestAuthority.Revision, Digest: latestAuthority.Digest, AuthorityNonce: secondChallenge.AuthorityNonce, FullSnapshotReceived: true}
	accepted, err := store.DesiredStateExchange(ctx, machine.MachineID, acknowledgement)
	if err != nil || accepted.AcknowledgedNonce != secondChallenge.AuthorityNonce {
		t.Fatalf("current authority handshake was not accepted: (%+v, %v)", accepted, err)
	}
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, protocol.StatusRequest{
		AuthorityEpoch: latestAuthority.Epoch, Revision: latestAuthority.Revision, Digest: latestAuthority.Digest,
		Received: true, Applied: true, AppliedRevision: latestAuthority.Revision, AppliedDigest: latestAuthority.Digest,
	}, now.Add(2500*time.Millisecond)); !errors.Is(err, ErrCoreNotImplemented) {
		t.Fatalf("Agent reported an unimplemented Core as applied: %v", err)
	}
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, protocol.StatusRequest{
		AuthorityEpoch: latestAuthority.Epoch, Revision: latestAuthority.Revision, Digest: latestAuthority.Digest,
		Received: false,
	}, now.Add(2600*time.Millisecond)); !errors.Is(err, ErrDesiredStateNotReceived) {
		t.Fatalf("Agent reported heartbeat without receiving desired-state: %v", err)
	}
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, protocol.StatusRequest{AuthorityEpoch: oldAuthority.Epoch, Revision: oldAuthority.Revision, Digest: oldAuthority.Digest}, now.Add(3*time.Second)); !errors.Is(err, ErrAuthorityMismatch) {
		t.Fatalf("old authority status changed current identity: %v", err)
	}
	if _, err := store.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{RequestID: "550e8400-e29b-41d4-a716-446655440004", AuthorityEpoch: latestAuthority.Epoch, Revision: latestAuthority.Revision, Digest: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("same-revision digest conflict result = %v", err)
	}
	if _, err := store.DesiredStateExchange(ctx, machine.MachineID, staleAcknowledgement); !errors.Is(err, ErrAuthorityMismatch) {
		t.Fatalf("old authority exchange changed current identity: %v", err)
	}
	stored, err := store.GetMachine(ctx, machine.MachineID)
	if err != nil || stored.AuthorityEpoch != latestAuthority.Epoch {
		t.Fatalf("machine authority was rolled back: (%q, %v)", stored.AuthorityEpoch, err)
	}
}

func openMachineTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCoreStatusKeepsGenerationSeparateFromAuthority(t *testing.T) {
	store := openMachineTestStore(t)
	ctx, now := context.Background(), time.Now().UTC()
	join, candidate, registrationID := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, authority, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registrationID), now)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := security.NewUUID()
	request := protocol.StatusRequest{AuthorityEpoch: authority.Epoch, Revision: authority.Revision, Digest: authority.Digest, Received: true,
		Applied: true, AppliedAuthorityEpoch: authority.Epoch, AppliedRevision: authority.Revision, AppliedDigest: authority.Digest, CoreGeneration: 2, CoreInstanceID: instance}
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, request, now); err != nil {
		t.Fatal(err)
	}
	request.CoreGeneration = 1
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, request, now); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("generation rollback: %v", err)
	}
	request.CoreGeneration = 2
	request.CoreInstanceID, _ = security.NewUUID()
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, request, now); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("same generation with different process: %v", err)
	}
	newAuthority, err := store.RotatePanelAuthority(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := store.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{AuthorityEpoch: authority.Epoch, Revision: authority.Revision, Digest: authority.Digest})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DesiredStateExchange(ctx, machine.MachineID, protocol.DesiredStateRequest{AuthorityEpoch: newAuthority.Epoch, Revision: newAuthority.Revision, Digest: newAuthority.Digest, AuthorityNonce: challenge.AuthorityNonce, FullSnapshotReceived: true})
	if err != nil {
		t.Fatal(err)
	}
	request.AuthorityEpoch, request.Revision, request.Digest = newAuthority.Epoch, newAuthority.Revision, newAuthority.Digest
	request.CoreGeneration, request.CoreInstanceID, request.Applied, request.ApplyError = 3, instance, false, protocol.ErrorCoreHealthFailed
	if _, err := store.RecordMachineStatus(ctx, machine.MachineID, request, now); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetMachine(ctx, machine.MachineID)
	if err != nil || stored.AuthorityEpoch != newAuthority.Epoch || stored.AppliedAuthorityEpoch != authority.Epoch || stored.CoreGeneration != 3 || stored.ApplyError != protocol.ErrorCoreHealthFailed {
		t.Fatalf("failed authority candidate overwrote previous applied scope: %v", err)
	}
}

func testSecrets(t *testing.T) (join, candidate, registration string) {
	t.Helper()
	var err error
	join, err = security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = security.NewToken(security.MachineSecretBytes)
	if err != nil {
		t.Fatal(err)
	}
	registration, err = security.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return
}

func testRegistration(join, candidate, registration string) RegisterMachineInput {
	return testRegistrationFor(join, candidate, registration, "")
}

func testRegistrationFor(join, candidate, registration, target string) RegisterMachineInput {
	return RegisterMachineInput{
		RegistrationID: registration, JoinTokenHash: security.HashToken(join), CandidateHash: security.HashToken(candidate),
		TargetMachineID: target, Hostname: "machine-one", DisplayName: "Machine One", OS: "linux", Architecture: "amd64",
		CPUCount: 4, MemoryBytes: 4 << 30, AgentVersion: protocol.AgentVersion,
	}
}
