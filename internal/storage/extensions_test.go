package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/security"
)

func TestExtensionLifecycleIsEncryptedFailClosedAndReferenceSafe(t *testing.T) {
	store := openMachineTestStore(t)
	ctx, now := context.Background(), time.Now().UTC()
	join, candidate, registration := testSecrets(t)
	if err := store.CreateJoinToken(ctx, security.HashToken(join), "", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.RegisterMachine(ctx, testRegistration(join, candidate, registration), now)
	if err != nil {
		t.Fatal(err)
	}
	pkg := extensionTestPackage()
	if _, err := store.InstallExtension(ctx, pkg, now); err != nil {
		t.Fatal(err)
	}
	certificate, privateKey := extensionTestCertificate(t)
	input := NodeInput{Name: "Trojan 扩展", PublicHost: "edge.example.test", PublicPort: 45443, ListenHost: "127.0.0.1", ListenPort: 19443, ServerName: "www.cloudflare.com", Enabled: true}
	assignment := &NodeExtensionInput{ID: pkg.Manifest.ID, Version: pkg.Manifest.Version, Values: map[string]string{"sni": "edge.example.test", "certificate_pem": certificate, "private_key_pem": privateKey}}
	if _, err := store.SaveNodeWithExtension(ctx, machine.MachineID, "", input, assignment, now); !errors.Is(err, ErrExtensionDisabled) {
		t.Fatalf("new Node accepted disabled extension: %v", err)
	}
	if err := store.SetExtensionEnabled(ctx, pkg.Manifest.ID, pkg.Manifest.Version, true, now); err != nil {
		t.Fatal(err)
	}
	node, err := store.SaveNodeWithExtension(ctx, machine.MachineID, "", input, assignment, now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil || len(state.Snapshot.Nodes) != 1 || state.Snapshot.Nodes[0].Extension == nil || state.Snapshot.Nodes[0].Extension.Kind != "trojan_tls" || !state.Snapshot.Nodes[0].Enabled {
		t.Fatalf("extension was not compiled into an enabled desired Node: %+v, %v", state.Snapshot, err)
	}
	var encrypted []byte
	if err := store.db.QueryRowContext(ctx, "SELECT encrypted_secrets FROM node_extensions WHERE node_id=?", node.NodeID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(certificate)) || bytes.Contains(encrypted, []byte(privateKey)) {
		t.Fatal("extension secret was stored in plaintext")
	}
	listed, err := store.ListNodes(ctx, machine.MachineID)
	if err != nil || len(listed) != 1 || listed[0].ExtensionKind != "trojan_tls" || listed[0].ExtensionSNI != "edge.example.test" {
		t.Fatalf("Node public extension projection invalid: %+v, %v", listed, err)
	}
	if err := store.SetExtensionEnabled(ctx, pkg.Manifest.ID, pkg.Manifest.Version, false, now); err != nil {
		t.Fatal(err)
	}
	state, err = store.GetMachineDesiredState(ctx, machine.MachineID)
	if err != nil || state.Snapshot.Nodes[0].Enabled {
		t.Fatalf("disabled extension still emitted an enabled Node: %+v, %v", state.Snapshot.Nodes, err)
	}
	if err := store.DeleteExtension(ctx, pkg.Manifest.ID, pkg.Manifest.Version); !errors.Is(err, ErrExtensionReferenced) {
		t.Fatalf("referenced extension deletion was accepted: %v", err)
	}
	if _, err := store.SaveNodeWithExtension(ctx, machine.MachineID, node.NodeID, input, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExtensionEnabled(ctx, pkg.Manifest.ID, pkg.Manifest.Version, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveNodeWithExtension(ctx, machine.MachineID, node.NodeID, input, assignment, now); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, machine.MachineID, node.NodeID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteExtension(ctx, pkg.Manifest.ID, pkg.Manifest.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallExtension(ctx, pkg, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallExtension(ctx, pkg, now); !errors.Is(err, ErrExtensionExists) {
		t.Fatalf("duplicate extension version was accepted: %v", err)
	}
}

func extensionTestPackage() extensions.Package {
	return extensions.Package{
		Manifest:  extensions.Manifest{ID: "trojan-tls-test", Version: "1.0.0", Name: "Trojan TLS test", Description: "fixed test package", Core: extensions.CoreSupport{Kind: "xray", MinVersion: "26.10.21"}, Capabilities: extensions.Capabilities{Direct: true, URI: true, Mihomo: true, Certificate: true}},
		Schema:    extensions.Schema{Fields: []extensions.Field{{Name: "sni", Type: "host", Label: "TLS SNI", Required: true, Max: 253}, {Name: "certificate_pem", Type: "pem", Label: "TLS 证书 PEM", Required: true, Secret: true, Max: 65536}, {Name: "private_key_pem", Type: "private_key_pem", Label: "TLS 私钥 PEM", Required: true, Secret: true, Max: 65536}}},
		Renderers: extensions.Renderers{Xray: extensions.XrayRenderer{Kind: "trojan_tls", Protocol: "trojan", Network: "tcp", TLS: true}, URI: extensions.SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}, Mihomo: extensions.SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}},
	}
}

func extensionTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "edge.example.test"}, DNSNames: []string{"edge.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
