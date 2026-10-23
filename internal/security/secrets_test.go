package security

import (
	"bytes"
	"testing"
)

func TestSecretsRejectOtherOwnersAndTampering(t *testing.T) {
	key := bytes.Repeat([]byte{3}, MasterKeySize)
	ciphertext, err := SealSecret(key, []byte("private fixture"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := OpenSecret(key, ciphertext, "node-a")
	if err != nil || string(plain) != "private fixture" || bytes.Contains(ciphertext, plain) {
		t.Fatalf("encrypted secret round trip failed: %v", err)
	}
	if _, err := OpenSecret(key, ciphertext, "node-b"); err == nil {
		t.Fatal("ciphertext was accepted for a different record")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := OpenSecret(key, ciphertext, "node-a"); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
