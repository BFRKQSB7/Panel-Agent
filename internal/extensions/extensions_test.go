package extensions

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPackageValidationCompilationAndRenderers(t *testing.T) {
	pkg := testPackage()
	certificate, privateKey := testCertificate(t)
	compiled, err := CompileNode(pkg, map[string]string{"sni": "edge.example.test", "certificate_pem": certificate, "private_key_pem": privateKey})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Kind != "trojan_tls" || compiled.SNI != "edge.example.test" || compiled.PrivateKeyPEM == "" {
		t.Fatal("compiled extension lost fixed or secret fields")
	}
	uri, err := RenderURI(pkg, "示例节点", "2001:db8::20", 443, "secret-value", compiled)
	if err != nil || !strings.Contains(uri, "trojan://secret-value@[2001:db8::20]:443") || !strings.Contains(uri, "sni=edge.example.test") {
		t.Fatalf("unexpected Trojan URI: %q, %v", uri, err)
	}
	mihomo, err := RenderMihomo(pkg, "示例节点", "edge.example.test", 443, "secret-value", compiled)
	if err != nil || mihomo["type"] != "trojan" || mihomo["tls"] != true || mihomo["udp"] != false {
		t.Fatalf("unexpected Mihomo renderer: %#v, %v", mihomo, err)
	}
	bad := pkg
	bad.Manifest.Description = "<script>alert(1)</script>"
	if err := Validate(bad); err == nil {
		t.Fatal("HTML-bearing package description accepted")
	}
	bad = pkg
	bad.Manifest.Core.MinVersion = "99.0.0"
	if err := Validate(bad); err == nil {
		t.Fatal("package requiring an unsupported Core version accepted")
	}
	badValues := map[string]string{"sni": "127.0.0.1", "certificate_pem": certificate, "private_key_pem": privateKey}
	if _, err := CompileNode(pkg, badValues); err == nil || strings.Contains(err.Error(), certificate) || strings.Contains(err.Error(), privateKey) {
		t.Fatalf("invalid secret-bearing compile error leaked data: %v", err)
	}
}

func TestImportZipRejectsTraversalLinksAndExtraFiles(t *testing.T) {
	data, err := json.Marshal(testPackage())
	if err != nil {
		t.Fatal(err)
	}
	valid := zipPackage(t, map[string]zipEntry{PackageFileName: {data: data}})
	if _, err := ImportZip(valid); err != nil {
		t.Fatalf("valid package rejected: %v", err)
	}
	traversal := zipPackage(t, map[string]zipEntry{"../package.json": {data: data}})
	if _, err := ImportZip(traversal); err == nil {
		t.Fatal("path traversal package accepted")
	}
	symlink := zipPackage(t, map[string]zipEntry{PackageFileName: {data: data}, "link": {data: []byte(PackageFileName), mode: os.ModeSymlink}})
	if _, err := ImportZip(symlink); err == nil {
		t.Fatal("symlink package accepted")
	}
	extra := zipPackage(t, map[string]zipEntry{PackageFileName: {data: data}, "README": {data: []byte("not executable")}})
	if _, err := ImportZip(extra); err == nil {
		t.Fatal("package with undeclared extra file accepted")
	}
}

type zipEntry struct {
	data []byte
	mode os.FileMode
}

func zipPackage(t *testing.T, entries map[string]zipEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testPackage() Package {
	return Package{
		Manifest:  Manifest{ID: "trojan-tls-v1", Version: "1.0.0", Name: "Trojan TLS", Description: "固定 Trojan/TCP/TLS 示例", Core: CoreSupport{Kind: "xray", MinVersion: "26.10.21"}, Capabilities: Capabilities{Direct: true, URI: true, Mihomo: true, Certificate: true}},
		Schema:    Schema{Fields: []Field{{Name: "sni", Type: "host", Label: "TLS SNI", Required: true, Max: 253}, {Name: "certificate_pem", Type: "pem", Label: "TLS 证书 PEM", Required: true, Secret: true, Max: 65536}, {Name: "private_key_pem", Type: "private_key_pem", Label: "TLS 私钥 PEM", Required: true, Secret: true, Max: 65536}}},
		Renderers: Renderers{Xray: XrayRenderer{Kind: "trojan_tls", Protocol: "trojan", Network: "tcp", TLS: true}, URI: SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}, Mihomo: SubscriptionRenderer{Kind: "trojan_tls", Enabled: true}},
	}
}

func TestOfficialHysteriaDeclarationAndSubscriptions(t *testing.T) {
	data, err := os.ReadFile("../../examples/extensions/official-hysteria2/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJSON(data); err != nil {
		t.Fatal(err)
	}
	pkg := testPackage()
	pkg.Manifest.ID = "official-hysteria2"
	pkg.Manifest.Core = CoreSupport{Kind: "hysteria", MinVersion: "2.13.0"}
	pkg.Renderers.Xray = XrayRenderer{}
	pkg.Renderers.Hysteria = &HysteriaRenderer{Kind: "hysteria2_tls", Version: 2}
	pkg.Renderers.URI.Kind = "hysteria2_tls"
	pkg.Renderers.Mihomo.Kind = "hysteria2_tls"
	cert, key := testCertificate(t)
	node, err := CompileNode(pkg, map[string]string{"sni": "edge.example.test", "certificate_pem": cert, "private_key_pem": key})
	if err != nil {
		t.Fatal(err)
	}
	uri, err := RenderURI(pkg, "hy2", "2001:db8::1", 45002, "a:b@secret", node)
	if err != nil || !strings.HasPrefix(uri, "hysteria2://a%3Ab%40secret@[2001:db8::1]:45002/") || !strings.Contains(uri, "insecure=0") {
		t.Fatalf("URI %s: %v", uri, err)
	}
	proxy, err := RenderMihomo(pkg, "hy2", "edge.example.test", 45002, "secret", node)
	if err != nil || proxy["type"] != "hysteria2" || proxy["skip-cert-verify"] != false {
		t.Fatalf("proxy %#v: %v", proxy, err)
	}
	pkg.Renderers.Xray = XrayRenderer{Kind: "trojan_tls", Protocol: "trojan", Network: "tcp", TLS: true}
	if err := Validate(pkg); err == nil {
		t.Fatal("simultaneous conflicting Core renderers accepted")
	}
}

func TestExtensionFullChainAndPEMBounds(t *testing.T) {
	cert, key := testCertificate(t)
	values := map[string]string{"sni": "edge.example.test", "certificate_pem": cert + cert, "private_key_pem": key}
	compiled, err := CompileNode(testPackage(), values)
	if err != nil || compiled.CertificatePEM != cert+cert {
		t.Fatalf("fullchain rejected or truncated: %v", err)
	}
	for _, invalid := range []string{cert + "not PEM", strings.Repeat(cert, 9), "garbage\n" + cert, strings.Repeat("x", 65537)} {
		values["certificate_pem"] = invalid
		if _, err := CompileNode(testPackage(), values); err == nil {
			t.Fatal("invalid certificate chain accepted")
		}
	}
}

func testCertificate(t *testing.T) (string, string) {
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
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certificate), string(privateKey)
}
