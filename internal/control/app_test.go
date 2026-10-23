package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/security"
)

func TestAdminAuthCSRFLogoutAndSettingsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := []byte("d02-test-password-2026")
	hash, err := security.HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dir, hash); err != nil {
		t.Fatal(err)
	}

	config := Config{DataDir: dir, Listen: "127.0.0.1:0"}
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	client, baseURL := newTestClient(t, httpServer.URL)

	response := request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("unauthenticated settings status = %d", response.StatusCode)
	}
	response.Body.Close()

	csrf := fetchLoginCSRF(t, client, baseURL)
	response = postForm(t, client, baseURL+"/login", url.Values{"csrf": {csrf}, "password": {"incorrect password"}}, baseURL)
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("wrong password status = %d", response.StatusCode)
	}
	response.Body.Close()

	csrf = fetchLoginCSRF(t, client, baseURL)
	response = postForm(t, client, baseURL+"/login", url.Values{"csrf": {csrf}, "password": {string(password)}}, baseURL)
	if response.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("valid login status = %d: %s", response.StatusCode, body)
	}
	response.Body.Close()

	response = request(t, client, http.MethodPut, baseURL+"/api/settings", strings.NewReader(`{"site_name":"Persisted D02"}`), baseURL)
	if response.StatusCode != http.StatusForbidden {
		response.Body.Close()
		t.Fatalf("settings update without CSRF status = %d", response.StatusCode)
	}
	response.Body.Close()

	csrf = csrfCookie(t, client, baseURL)
	response = putSettings(t, client, baseURL, "Persisted D02", csrf, "http://attacker.invalid")
	if response.StatusCode != http.StatusForbidden {
		response.Body.Close()
		t.Fatalf("settings update with mismatched origin status = %d", response.StatusCode)
	}
	response.Body.Close()

	response = putSettings(t, client, baseURL, "Persisted D02", csrf, baseURL)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("settings update status = %d: %s", response.StatusCode, body)
	}
	response.Body.Close()

	unauthenticated, _ := newTestClient(t, baseURL)
	response = request(t, unauthenticated, http.MethodPost, baseURL+"/api/init", strings.NewReader(`{"password":"attacker"}`), baseURL)
	if response.StatusCode != http.StatusMethodNotAllowed {
		response.Body.Close()
		t.Fatalf("unexpected initialization route status = %d", response.StatusCode)
	}
	response.Body.Close()

	httpServer.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	server, err = NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer = httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()
	baseURL = httpServer.URL

	response = request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("session remained valid after restart: status %d", response.StatusCode)
	}
	response.Body.Close()

	csrf = fetchLoginCSRF(t, client, baseURL)
	response = postForm(t, client, baseURL+"/login", url.Values{"csrf": {csrf}, "password": {string(password)}}, baseURL)
	if response.StatusCode != http.StatusSeeOther {
		response.Body.Close()
		t.Fatalf("login after restart status = %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	var settings map[string]string
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("settings read after restart status = %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if settings["site_name"] != "Persisted D02" {
		t.Fatalf("saved setting after restart = %q", settings["site_name"])
	}

	csrf = csrfCookie(t, client, baseURL)
	response = postForm(t, client, baseURL+"/logout", url.Values{"csrf": {csrf}}, baseURL)
	if response.StatusCode != http.StatusSeeOther {
		response.Body.Close()
		t.Fatalf("logout status = %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, client, http.MethodGet, baseURL+"/api/settings", nil, "")
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("session remained valid after logout: status %d", response.StatusCode)
	}
	response.Body.Close()
}

func TestConfigRequiresTLSOutsideLoopback(t *testing.T) {
	if _, err := (Config{DataDir: t.TempDir(), Listen: "0.0.0.0:8080"}).validate(); err == nil {
		t.Fatal("non-loopback plaintext listener was accepted")
	}
	proxyConfig := Config{DataDir: t.TempDir(), Listen: "127.0.0.1:8080", TrustProxy: true}
	if _, err := proxyConfig.validate(); err != nil || !proxyConfig.secureCookies() {
		t.Fatalf("loopback trusted-proxy configuration rejected or cookies insecure: %v", err)
	}
	if _, err := (Config{DataDir: t.TempDir(), Listen: "127.0.0.1:8080", CookieSecure: true}).validate(); err == nil {
		t.Fatal("secure-cookie mode without HTTPS was accepted")
	}
	if _, err := (Config{DataDir: t.TempDir(), Listen: "127.0.0.1:8080"}).validate(); err != nil {
		t.Fatalf("loopback development listener rejected: %v", err)
	}
}

func newTestClient(t *testing.T, baseURL string) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, baseURL
}

func fetchLoginCSRF(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	response := request(t, client, http.MethodGet, baseURL+"/login", nil, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login page status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("login page did not include a CSRF token")
	}
	return string(match[1])
}

func csrfCookie(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == csrfCookieName {
			return cookie.Value
		}
	}
	t.Fatal("CSRF cookie missing")
	return ""
}

func postForm(t *testing.T, client *http.Client, endpoint string, values url.Values, origin string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func putSettings(t *testing.T, client *http.Client, baseURL, siteName, csrf, origin string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"site_name": siteName})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, baseURL+"/api/settings", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func request(t *testing.T, client *http.Client, method, endpoint string, body io.Reader, origin string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRateLimiterLocksAfterFiveFailures(t *testing.T) {
	limiter := newLoginLimiter()
	now := time.Now()
	for i := 0; i < maxLoginFailures-1; i++ {
		if wait := limiter.Failed("127.0.0.1", now); wait != 0 {
			t.Fatalf("unexpected lockout after %d failures", i+1)
		}
	}
	limiter.Failed("127.0.0.1", now)
	if allowed, wait := limiter.Allowed("127.0.0.1", now); allowed || wait <= 0 {
		t.Fatal("fifth failure did not apply a lockout")
	}
}
