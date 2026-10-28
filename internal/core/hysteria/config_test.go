package hysteria

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func serverOptions(t *testing.T) ServerOptions {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hy2.example.com"}, DNSNames: []string{"hy2.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return ServerOptions{
		ListenHost: "0.0.0.0", ListenPort: 24443, ServerName: "hy2.example.com",
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		InstanceDir:    "/var/lib/panel-agent/agent/hysteria/node-1", AuthAddress: "127.0.0.1:11000", AuthRouteToken: testSecret,
		StatsAddress: "127.0.0.1:11001", StatsSecret: strings.Repeat("b", 32),
	}
}

func TestRenderOfficialServer(t *testing.T) {
	opts := serverOptions(t)
	files, err := RenderServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Listen string `json:"listen"`
		TLS    struct {
			Cert     string `json:"cert"`
			Key      string `json:"key"`
			SNIGuard string `json:"sniGuard"`
		} `json:"tls"`
		Auth struct {
			Type string `json:"type"`
			HTTP struct {
				URL string `json:"url"`
			} `json:"http"`
		} `json:"auth"`
		Stats struct {
			Listen string `json:"listen"`
			Secret string `json:"secret"`
		} `json:"trafficStats"`
	}
	if err := json.Unmarshal(files.Config, &config); err != nil {
		t.Fatal(err)
	}
	if config.Listen != "0.0.0.0:24443" || config.Auth.Type != "http" || config.Auth.HTTP.URL != "http://127.0.0.1:11000/auth/"+testSecret || config.Stats.Listen != opts.StatsAddress || config.Stats.Secret != opts.StatsSecret {
		t.Fatal("official config mapping changed")
	}
	if config.TLS.SNIGuard != "strict" || config.TLS.Key != opts.InstanceDir+"/server.key" || config.TLS.Cert != opts.InstanceDir+"/server.crt" {
		t.Fatal("unsafe TLS config")
	}
	if string(files.PrivateKey) != opts.PrivateKeyPEM || string(files.Certificate) != opts.CertificatePEM || strings.Contains(string(files.Config), "BEGIN PRIVATE KEY") {
		t.Fatal("TLS files not isolated")
	}
	for name, change := range map[string]func(*ServerOptions){
		"remote API":        func(o *ServerOptions) { o.StatsAddress = "192.0.2.1:11001" },
		"wrong certificate": func(o *ServerOptions) { o.ServerName = "other.example.com" },
		"same TCP port":     func(o *ServerOptions) { o.StatsAddress = "[::1]:11000" },
		"path traversal":    func(o *ServerOptions) { o.InstanceDir = "/tmp/../etc" },
		"unsafe auth URL":   func(o *ServerOptions) { o.AuthRouteToken = strings.Repeat("a", 32) + "&?" },
		"shared secret":     func(o *ServerOptions) { o.StatsSecret = o.AuthRouteToken },
	} {
		t.Run(name, func(t *testing.T) {
			bad := opts
			change(&bad)
			if _, err := RenderServer(bad); err == nil {
				t.Fatal("invalid server accepted")
			}
		})
	}
}
