package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/security"
)

func TestTokenFamiliesHashOnlyRevocationAndPartialRetry(t *testing.T) {
	s := openMachineTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	group, err := s.SaveGroup(ctx, "user", "", GroupInput{Name: "token test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.SaveUser(ctx, "", UserInput{Name: "links", UserGroupID: group.GroupID, Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := security.NewUUID()
	dashboard, err := s.ResetUserToken(ctx, op, "dashboard", TokenMutation{UserID: user.UserID, Spec: security.CapabilitySpec{Prefix: "dash"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := s.ResetUserToken(ctx, op, "subscription", TokenMutation{UserID: user.UserID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Token == subscription.Token || dashboard.Token == user.CredentialUUID {
		t.Fatal("capability and protocol credentials are not independent")
	}
	if _, err := s.UserForCapability(ctx, "subscription", dashboard.Token); !errors.Is(err, ErrCapabilityNotFound) {
		t.Fatal("dashboard token authenticated subscription")
	}
	var persisted string
	if err := s.db.QueryRowContext(ctx, "SELECT token_hash FROM user_tokens WHERE user_id=? AND kind='dashboard' AND revoked_at IS NULL", user.UserID).Scan(&persisted); err != nil || persisted == dashboard.Token || !security.IsValidDigest(persisted) {
		t.Fatal("plaintext token stored", err)
	}
	var allRows []byte
	rows, err := s.db.QueryContext(ctx, "SELECT token_hash FROM token_operations")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		allRows = append(allRows, []byte(hash)...)
	}
	rows.Close()
	if bytes.Contains(allRows, []byte(dashboard.Token)) || bytes.Contains(allRows, []byte(subscription.Token)) {
		t.Fatal("operation receipt retains plaintext")
	}
	retry, err := s.ResetUserToken(ctx, op, "subscription", TokenMutation{UserID: user.UserID, Spec: security.CapabilitySpec{Value: "weak"}}, now)
	if err != nil || !retry.AlreadyApplied || retry.Token != "" || retry.Generation != 1 {
		t.Fatal("retry rotated or redisclosed a successful item", err)
	}
	op2, _ := security.NewUUID()
	rotated, err := s.ResetUserToken(ctx, op2, "subscription", TokenMutation{UserID: user.UserID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForCapability(ctx, "subscription", subscription.Token); !errors.Is(err, ErrCapabilityNotFound) {
		t.Fatal("old subscription URL still authenticates")
	}
	if id, err := s.UserForCapability(ctx, "dashboard", dashboard.Token); err != nil || id != user.UserID {
		t.Fatal("subscription reset revoked dashboard token", err)
	}
	after, err := s.GetUser(ctx, user.UserID)
	if err != nil || after.CredentialUUID != user.CredentialUUID {
		t.Fatal("subscription reset rotated protocol UUID", err)
	}
	op3, _ := security.NewUUID()
	if _, err := s.ResetUserToken(ctx, op3, "subscription", TokenMutation{UserID: user.UserID, Spec: security.CapabilitySpec{Value: dashboard.Token}}, now); !errors.Is(err, ErrCapabilityConflict) {
		t.Fatal("cross-family value collision accepted", err)
	}
	if _, err := s.ResetUserToken(ctx, op3, "subscription", TokenMutation{UserID: user.UserID, Spec: security.CapabilitySpec{Value: subscription.Token}}, now); !errors.Is(err, ErrCapabilityConflict) {
		t.Fatal("retired token was reused", err)
	}
	op4, _ := security.NewUUID()
	results, err := s.ResetUserTokens(ctx, op4, "dashboard", []TokenMutation{{UserID: user.UserID}, {UserID: "missing-user"}}, now)
	if err != nil || results[0].Token == "" || results[1].Error != "user_not_found" {
		t.Fatal("partial batch did not preserve outcomes", err)
	}
	first := results[0]
	results, err = s.ResetUserTokens(ctx, op4, "dashboard", []TokenMutation{{UserID: user.UserID}, {UserID: "missing-user"}}, now)
	if err != nil || !results[0].AlreadyApplied || results[0].Token != "" || results[0].Generation != first.Generation {
		t.Fatal("partial retry repeated successful token rotation", err)
	}
	if _, err := s.UserForCapability(ctx, "subscription", rotated.Token); err != nil {
		t.Fatal("dashboard reset revoked subscription token", err)
	}
	if err := s.DeleteUser(ctx, user.UserID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForCapability(ctx, "subscription", rotated.Token); !errors.Is(err, ErrCapabilityNotFound) {
		t.Fatal("deleted user link remains active")
	}
}
