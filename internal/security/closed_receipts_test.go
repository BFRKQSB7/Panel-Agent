package security

import (
	"bytes"
	"testing"
)

func TestClosedReceiptScopeTamperAndRestore(t *testing.T) {
	key := bytes.Repeat([]byte{17}, MasterKeySize)
	c := ClosedClaims{Version: 1, InstallationID: "11111111-1111-4111-8111-111111111111", MachineID: "22222222-2222-4222-8222-222222222222", LedgerEpoch: "33333333-3333-4333-8333-333333333333", ClosedThrough: 9007199254740993}
	r, err := SignClosedReceipt(key, c)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyClosedReceipt(key, r, c.InstallationID, c.MachineID, c.LedgerEpoch) != nil {
		t.Fatal("restored same master key receipt rejected")
	}
	mutations := []func(*ClosedReceipt){func(r *ClosedReceipt) { r.Version++ }, func(r *ClosedReceipt) { r.ClosedThrough++ }, func(r *ClosedReceipt) { r.InstallationID = r.MachineID }, func(r *ClosedReceipt) { r.MachineID = r.LedgerEpoch }, func(r *ClosedReceipt) { r.LedgerEpoch = r.InstallationID }, func(r *ClosedReceipt) { r.MAC = "invalid" }}
	for _, mutate := range mutations {
		bad := r
		mutate(&bad)
		if VerifyClosedReceipt(key, bad, c.InstallationID, c.MachineID, c.LedgerEpoch) == nil {
			t.Fatal("tampered receipt accepted")
		}
	}
	if VerifyClosedReceipt(bytes.Repeat([]byte{18}, 32), r, c.InstallationID, c.MachineID, c.LedgerEpoch) == nil {
		t.Fatal("wrong installation key accepted")
	}
	if _, err := SignClosedReceipt(key, ClosedClaims{}); err == nil {
		t.Fatal("empty claims accepted")
	}
}

func TestClosedOrdinalProofCanonicalAndAuthenticated(t *testing.T) {
	key := bytes.Repeat([]byte{17}, MasterKeySize)
	claims := ClosedClaims{Version: 2, InstallationID: "11111111-1111-4111-8111-111111111111", MachineID: "22222222-2222-4222-8222-222222222222", LedgerEpoch: "33333333-3333-4333-8333-333333333333", ClosedThrough: 10, ClosedOrdinals: "1-3,5-9"}
	r, err := SignClosedReceipt(key, claims)
	if err != nil || VerifyClosedReceipt(key, r, claims.InstallationID, claims.MachineID, claims.LedgerEpoch) != nil {
		t.Fatalf("proof %v", err)
	}
	r.ClosedOrdinals = "1-9"
	if VerifyClosedReceipt(key, r, claims.InstallationID, claims.MachineID, claims.LedgerEpoch) == nil {
		t.Fatal("forged coverage accepted")
	}
	for _, value := range []string{"", "0-1", "01-2", "2-1", "1-2,3-4", "5-9,1-3", "1-281474976710656"} {
		claims.ClosedOrdinals = value
		if _, err := SignClosedReceipt(key, claims); err == nil {
			t.Fatalf("noncanonical proof %q", value)
		}
	}
}
