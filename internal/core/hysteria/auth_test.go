package hysteria

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthExpiryRotationAndSnapshotIsolation(t *testing.T) {
	auth, err := NewAuthenticator(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	request := func(secret string) bool {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"auth": secret, "addr": "192.0.2.1:5000", "tx": 100})
		r := httptest.NewRequest("POST", auth.Path(), strings.NewReader(string(body)))
		r.RemoteAddr = "127.0.0.1:30000"
		w := httptest.NewRecorder()
		auth.ServeHTTP(w, r)
		var result struct {
			OK bool   `json:"ok"`
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.OK && result.ID != "" {
			t.Fatal("denial leaked identity")
		}
		return result.OK
	}
	if request(testSecret) {
		t.Fatal("empty backend allowed user")
	}
	accounts := []Account{{ID: "user-node", Secret: testSecret}}
	if _, err := auth.Replace(accounts); err != nil {
		t.Fatal(err)
	}
	accounts[0].ExpiresAt = time.Now().Add(-time.Hour)
	if !request(testSecret) {
		t.Fatal("caller mutated active snapshot")
	}
	revoked, err := auth.Replace(accounts)
	if err != nil || len(revoked) != 1 || revoked[0] != "user-node" {
		t.Fatal("expiry change did not revoke existing identity")
	}
	if request(testSecret) {
		t.Fatal("expired user allowed")
	}
	accounts[0].ExpiresAt = time.Time{}
	accounts[0].Secret = strings.Repeat("b", 32)
	if _, err := auth.Replace(accounts); err != nil {
		t.Fatal(err)
	}
	if request(testSecret) || !request(accounts[0].Secret) {
		t.Fatal("rotation failed")
	}
	if _, err := auth.Replace(append(accounts, accounts[0])); err == nil {
		t.Fatal("duplicate allowed")
	}
	if _, err := auth.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if request(accounts[0].Secret) {
		t.Fatal("revoked user allowed")
	}
}

func TestAuthRejectsRemoteAndOversizedRequests(t *testing.T) {
	auth, err := NewAuthenticator(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote, body string
		status       int
	}{
		{"192.0.2.1:1000", `{}`, 404},
		{"127.0.0.1:1000", strings.Repeat(" ", 4097), 400},
		{"127.0.0.1:1000", `{} {}`, 400},
	} {
		r := httptest.NewRequest("POST", auth.Path(), strings.NewReader(tc.body))
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		auth.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d", w.Code, tc.status)
		}
	}
}
