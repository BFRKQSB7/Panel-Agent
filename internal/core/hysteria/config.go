package hysteria

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"path"
	"strconv"
	"strings"
)

// ServerOptions is supplied by the Agent, not an arbitrary extension template.
// File ownership, atomic writes and process isolation belong to the supervisor.
type ServerOptions struct {
	ListenHost     string
	ListenPort     int
	ServerName     string
	CertificatePEM string
	PrivateKeyPEM  string
	InstanceDir    string
	AuthAddress    string
	AuthRouteToken string
	StatsAddress   string
	StatsSecret    string
}

type ServerFiles struct {
	Config      []byte
	Certificate []byte
	PrivateKey  []byte
}

// RenderServer produces JSON accepted by the official executable. It does not
// start a process, open a listener, write secrets, or claim runtime validation.
func RenderServer(o ServerOptions) (ServerFiles, error) {
	if net.ParseIP(o.ListenHost) == nil || o.ListenPort < 1 || o.ListenPort > 65535 {
		return ServerFiles{}, errors.New("hysteria: invalid UDP listener")
	}
	if !path.IsAbs(o.InstanceDir) || path.Clean(o.InstanceDir) != o.InstanceDir || o.InstanceDir == "/" || strings.ContainsAny(o.InstanceDir, "\x00\r\n\\") {
		return ServerFiles{}, errors.New("hysteria: instance directory must be a clean absolute Linux path")
	}
	if len(o.CertificatePEM) > 65536 || len(o.PrivateKeyPEM) > 65536 || o.ServerName == "" || len(o.ServerName) > 253 {
		return ServerFiles{}, errors.New("hysteria: invalid TLS material")
	}
	pair, err := tls.X509KeyPair([]byte(o.CertificatePEM), []byte(o.PrivateKeyPEM))
	if err != nil {
		return ServerFiles{}, errors.New("hysteria: invalid TLS certificate/key pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(o.ServerName) != nil {
		return ServerFiles{}, errors.New("hysteria: certificate does not match server name")
	}
	if err := validateEndpoint(o.AuthAddress); err != nil {
		return ServerFiles{}, err
	}
	if err := validateEndpoint(o.StatsAddress); err != nil {
		return ServerFiles{}, err
	}
	// Conservative separation also avoids wildcard/dual-stack TCP collisions.
	_, authPort, _ := net.SplitHostPort(o.AuthAddress)
	_, statsPort, _ := net.SplitHostPort(o.StatsAddress)
	if authPort == statsPort {
		return ServerFiles{}, errors.New("hysteria: auth and stats require distinct ports")
	}
	auth, err := NewAuthenticator(o.AuthRouteToken)
	if err != nil {
		return ServerFiles{}, err
	}
	if !validSecret(o.StatsSecret) || o.StatsSecret == o.AuthRouteToken {
		return ServerFiles{}, errors.New("hysteria: independent stats secret required")
	}
	config := map[string]any{
		"listen":       net.JoinHostPort(o.ListenHost, strconv.Itoa(o.ListenPort)),
		"tls":          map[string]any{"cert": path.Join(o.InstanceDir, "server.crt"), "key": path.Join(o.InstanceDir, "server.key"), "sniGuard": "strict"},
		"auth":         map[string]any{"type": "http", "http": map[string]any{"url": "http://" + o.AuthAddress + auth.Path()}},
		"trafficStats": map[string]any{"listen": o.StatsAddress, "secret": o.StatsSecret},
		"disableUDP":   false,
		"speedTest":    false,
		// Keep proxy traffic away from local control APIs and private services.
		"acl": map[string]any{"inline": []string{
			"reject(0.0.0.0/8)", "reject(10.0.0.0/8)", "reject(100.64.0.0/10)", "reject(127.0.0.0/8)",
			"reject(169.254.0.0/16)", "reject(172.16.0.0/12)", "reject(192.168.0.0/16)",
			"reject(224.0.0.0/4)", "reject(240.0.0.0/4)", "reject(::/128)", "reject(::1/128)",
			"reject(::ffff:0:0/96)", "reject(fc00::/7)", "reject(fe80::/10)", "reject(ff00::/8)", "direct(all)",
		}},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ServerFiles{}, err
	}
	return ServerFiles{Config: data, Certificate: []byte(o.CertificatePEM), PrivateKey: []byte(o.PrivateKeyPEM)}, nil
}
