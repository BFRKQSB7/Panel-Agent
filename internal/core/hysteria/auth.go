package hysteria

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type Account struct {
	// ID is an opaque User x Node mapping, never a password or subscription token.
	ID        string
	Secret    string
	ExpiresAt time.Time
}

// Authenticator is an instance-local, deny-by-default HTTP authentication backend.
// The owner supplies only accounts allowed by the applied permission/quota state.
// It must supervise the core and revoke existing sessions separately.
type Authenticator struct {
	mu       sync.RWMutex
	route    string
	accounts map[[32]byte]Account
}

func NewAuthenticator(routeToken string) (*Authenticator, error) {
	if !validSecret(routeToken) {
		return nil, errors.New("hysteria: invalid auth route token")
	}
	for _, c := range routeToken {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, errors.New("hysteria: auth route token must be URL-safe")
		}
	}
	return &Authenticator{route: "/auth/" + routeToken, accounts: map[[32]byte]Account{}}, nil
}

func (a *Authenticator) Path() string { return a.route }

// Replace swaps a complete validated snapshot. The returned IDs need session
// revocation, including accounts whose secret or expiry changed. Do not ACK a
// desired state until those sessions and concurrent auth requests are fenced.
func (a *Authenticator) Replace(accounts []Account) ([]string, error) {
	if len(accounts) > 4096 {
		return nil, errors.New("hysteria: too many accounts")
	}
	next := make(map[[32]byte]Account, len(accounts))
	ids := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		if !validID(account.ID) || !validSecret(account.Secret) || ids[account.ID] {
			return nil, errors.New("hysteria: invalid or duplicate account")
		}
		key := sha256.Sum256([]byte(account.Secret))
		if _, exists := next[key]; exists {
			return nil, errors.New("hysteria: duplicate credential")
		}
		ids[account.ID] = true
		account.Secret = ""
		next[key] = account
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	revoked := []string{}
	for key, old := range a.accounts {
		current, exists := next[key]
		if !exists || current.ID != old.ID || !current.ExpiresAt.Equal(old.ExpiresAt) {
			revoked = append(revoked, old.ID)
		}
	}
	a.accounts = next
	return revoked, nil
}

func (a *Authenticator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(a.route)) != 1 || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	var input struct {
		Addr string `json:"addr"`
		Auth string `json:"auth"`
		TX   uint64 `json:"tx"`
	}
	d := json.NewDecoder(r.Body)
	if err := d.Decode(&input); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var response struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if validSecret(input.Auth) {
		a.mu.RLock()
		account, found := a.accounts[sha256.Sum256([]byte(input.Auth))]
		if found && (account.ExpiresAt.IsZero() || time.Now().Before(account.ExpiresAt)) {
			response.OK, response.ID = true, account.ID
		}
		a.mu.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
