package security

import (
	"strings"
	"testing"
	"time"
)

func TestCapabilitiesRejectWeakCustomValuesAndBindActions(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("a", 64), strings.Repeat("aB9_", 16), "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"} {
		if _, err := NewCapability(CapabilitySpec{Value: value}); err == nil {
			t.Fatal("weak custom capability accepted")
		}
	}
	random, err := NewCapability(CapabilitySpec{Prefix: "nyro"})
	if err != nil || !strings.HasPrefix(random, "nyro_") || !CapabilitySyntax(random) {
		t.Fatal("readable prefix lost random suffix", err)
	}
	custom, _ := NewToken(48)
	if _, err := NewCapability(CapabilitySpec{Value: custom}); err != nil {
		t.Fatal("random full custom value rejected", err)
	}
	master := make([]byte, 32)
	master[0] = 42
	signer, err := NewActionSigner(master)
	if err != nil {
		t.Fatal(err)
	}
	defer signer.Close()
	restarted, err := NewActionSigner(master)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	now := time.Now()
	token, err := signer.Issue("subscription-reset", "user:scope", now)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := restarted.Verify(token, "subscription-reset", "user:scope", now); err != nil || !IsUUIDv4(id) {
		t.Fatal("normal process restart broke action retry", err)
	}
	for _, input := range []struct {
		token, purpose, subject string
		at                      time.Time
	}{{token, "dashboard-reset", "user:scope", now}, {token, "subscription-reset", "other-user", now}, {token, "subscription-reset", "user:scope", now.Add(16 * time.Minute)}, {token + "x", "subscription-reset", "user:scope", now}} {
		if _, err := signer.Verify(input.token, input.purpose, input.subject, input.at); err == nil {
			t.Fatal("tampered, expired, or cross-purpose action accepted")
		}
	}
}
