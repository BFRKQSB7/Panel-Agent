package backup

import (
	"bytes"
	"errors"
	"testing"
)

func TestBundleRoundTripAndPasswordBoundaries(t *testing.T) {
	input := Input{
		Database:       []byte("sqlite snapshot"),
		MasterKey:      bytes.Repeat([]byte{0x42}, 32),
		SchemaVersion:  11,
		InstallationID: "11111111-1111-4111-8111-111111111111",
		Config:         ConfigSnapshot{DataDir: "/var/lib/panel-agent", Listen: "127.0.0.1:8080"},
		Extensions:     map[string][]byte{"trojan-tls@1.0.0": []byte(`{"manifest":{"id":"trojan-tls"}`)},
		Password:       "correct horse battery staple",
	}
	encoded, manifest, err := Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Magic != Magic || manifest.Version != FormatVersion || len(manifest.Extensions) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	bundle, err := Open(encoded, input.Password)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundle.Database, input.Database) || !bytes.Equal(bundle.MasterKey, input.MasterKey) || !bytes.Equal(bundle.Extensions["trojan-tls@1.0.0"], input.Extensions["trojan-tls@1.0.0"]) {
		t.Fatal("round trip changed backup contents")
	}
	if _, err := Open(encoded, "wrong password"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("wrong password error = %v", err)
	}
	if _, err := Open(encoded, ""); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("missing password error = %v", err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := Open(encoded, input.Password); err == nil {
		t.Fatal("tampered envelope was accepted")
	}
}

func TestUnencryptedBundleDoesNotAcceptMalformedEncryptionMetadata(t *testing.T) {
	encoded, _, err := Create(Input{
		Database:       []byte("db"),
		MasterKey:      bytes.Repeat([]byte{0x11}, 32),
		SchemaVersion:  1,
		InstallationID: "22222222-2222-4222-8222-222222222222",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(encoded, "unexpected"); err != nil {
		t.Fatalf("unencrypted bundle with ignored password = %v", err)
	}
}
