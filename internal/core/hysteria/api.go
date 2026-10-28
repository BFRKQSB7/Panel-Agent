// Package hysteria adapts the official Hysteria 2 executable's local APIs.
// It does not embed a proxy implementation or execute extension scripts.
package hysteria

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// Counter follows Hysteria's client perspective: TX is upload, RX download.
// These are cumulative process-local observations, not durable billing totals.
type Counter struct {
	TX int64 `json:"tx"`
	RX int64 `json:"rx"`
}

type API struct {
	baseURL string
	secret  string
	client  *http.Client
}

func validateEndpoint(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("hysteria: invalid local endpoint")
	}
	ip := net.ParseIP(host)
	port, err := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return errors.New("hysteria: endpoint must use a loopback IP and explicit port")
	}
	return nil
}

func validSecret(secret string) bool {
	if len(secret) < 32 || len(secret) > 256 {
		return false
	}
	for _, c := range secret {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func NewAPI(address, secret string) (*API, error) {
	if err := validateEndpoint(address); err != nil {
		return nil, err
	}
	if !validSecret(secret) {
		return nil, errors.New("hysteria: invalid API secret")
	}
	// Never use environment proxies or forward the API secret on redirects.
	transport := &http.Transport{
		DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
	}
	return &API{baseURL: "http://" + address, secret: secret, client: &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (a *API) Close() {
	if a != nil && a.client != nil {
		a.client.CloseIdleConnections()
	}
}

func (a *API) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	if ctx == nil || a == nil || a.client == nil {
		return nil, errors.New("hysteria: missing API client or context")
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("hysteria: invalid API request")
	}
	req.Header.Set("Authorization", a.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hysteria: local API request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hysteria: local API status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("hysteria: cannot read API response")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("hysteria: API response too large")
	}
	return data, nil
}

// Traffic never uses clear=1: a lost HTTP response must not discard usage.
func (a *API) Traffic(ctx context.Context) (map[string]Counter, error) {
	data, err := a.request(ctx, http.MethodGet, "/traffic", nil)
	if err != nil {
		return nil, err
	}
	// Decode entries explicitly to reject duplicate identities and missing values.
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("hysteria: invalid traffic object")
	}
	result := map[string]Counter{}
	for d.More() {
		token, err := d.Token()
		id, ok := token.(string)
		if err != nil || !ok || !validID(id) {
			return nil, errors.New("hysteria: invalid traffic identity")
		}
		if _, exists := result[id]; exists {
			return nil, errors.New("hysteria: duplicate traffic identity")
		}
		var raw struct {
			TX *int64 `json:"tx"`
			RX *int64 `json:"rx"`
		}
		if err := d.Decode(&raw); err != nil || raw.TX == nil || raw.RX == nil || *raw.TX < 0 || *raw.RX < 0 {
			return nil, errors.New("hysteria: invalid traffic counters")
		}
		result[id] = Counter{TX: *raw.TX, RX: *raw.RX}
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("hysteria: incomplete traffic object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("hysteria: trailing traffic data")
	}
	return result, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 256 || strings.TrimSpace(id) != id {
		return false
	}
	for _, c := range id {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

// Kick only terminates current sessions. The caller must first revoke auth and
// reconcile in-flight authentications; success is not proof of permission removal.
func (a *API) Kick(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 4096 {
		return errors.New("hysteria: too many kick identities")
	}
	for _, id := range ids {
		if !validID(id) {
			return errors.New("hysteria: invalid kick identity")
		}
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = a.request(ctx, http.MethodPost, "/kick", body)
	return err
}
