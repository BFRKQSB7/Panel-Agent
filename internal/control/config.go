package control

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DataDir      string
	Listen       string
	TLSCert      string
	TLSKey       string
	TrustProxy   bool
	CookieSecure bool
}

func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DataDir: os.Getenv("PANEL_DATA_DIR"),
		Listen:  os.Getenv("PANEL_LISTEN_ADDR"),
		TLSCert: os.Getenv("PANEL_TLS_CERT"),
		TLSKey:  os.Getenv("PANEL_TLS_KEY"),
	}
	if config.DataDir == "" {
		config.DataDir = "./data"
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8080"
	}
	var err error
	if value := os.Getenv("PANEL_TRUST_PROXY"); value != "" {
		config.TrustProxy, err = strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("PANEL_TRUST_PROXY must be true or false")
		}
	}
	if value := os.Getenv("PANEL_COOKIE_SECURE"); value != "" {
		config.CookieSecure, err = strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("PANEL_COOKIE_SECURE must be true or false")
		}
	}
	return config, nil
}

func (c Config) validate() (*tls.Config, error) {
	if strings.TrimSpace(c.DataDir) == "" {
		return nil, fmt.Errorf("data directory is required")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen address must be host:port")
	}
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	hasCert := c.TLSCert != ""
	hasKey := c.TLSKey != ""
	if hasCert != hasKey {
		return nil, fmt.Errorf("TLS certificate and key must be configured together")
	}
	if !hasCert && !loopback {
		return nil, fmt.Errorf("non-loopback management listeners require HTTPS")
	}
	if c.TrustProxy && !loopback {
		return nil, fmt.Errorf("trusted-proxy mode requires a loopback listener")
	}
	if c.CookieSecure && !hasCert && !c.TrustProxy {
		return nil, fmt.Errorf("secure cookies require HTTPS or trusted-proxy mode")
	}
	if !hasCert {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load HTTPS certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

func (c Config) secureCookies() bool {
	return c.TLSCert != "" || c.TrustProxy || c.CookieSecure
}
