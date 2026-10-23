package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestUserGroupAdminFormsPartialBatchAndAPIScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := "d05-user-admin-password"
	hash, err := security.HashAdminPassword([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAdmin(dir, hash); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewTLSServer(s.Handler())
	defer h.Close()
	admin, base := newTestClient(t, h.URL)
	admin.Transport = h.Client().Transport
	loginControlAdmin(t, admin, base, password)
	csrf := fetchMachinesCSRF(t, admin, base)
	form := url.Values{"csrf": {csrf}, "name": {"普通用户组"}}
	response := postForm(t, admin, base+"/admin/groups/user", form, base)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("group form=%d", response.StatusCode)
	}
	groups, err := s.store.ListGroups(context.Background(), "user")
	if err != nil || len(groups) != 1 {
		t.Fatal("group form did not persist", err)
	}
	form = url.Values{"csrf": {csrf}, "name": {"Alice"}, "user_group_id": {groups[0].GroupID}, "enabled": {"true"}, "expires_at": {"2030-01-01T00:00:00"}, "quota_bytes": {"0"}}
	response = postForm(t, admin, base+"/admin/users", form, base)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("user form=%d", response.StatusCode)
	}
	users, err := s.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 || users[0].QuotaBytes == nil || *users[0].QuotaBytes != 0 || users[0].ExpiresAt != "2030-01-01T00:00:00Z" {
		t.Fatal("form fields were conflated", err)
	}
	user := users[0]
	form = url.Values{"csrf": {csrf}, "names": {"Batch A\nAlice\nBatch B"}, "user_group_id": {groups[0].GroupID}, "enabled": {"true"}}
	response = postForm(t, admin, base+"/admin/users/batch/create", form, base)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("name_conflict")) || !bytes.Contains(body, []byte("Batch A")) || !bytes.Contains(body, []byte("Batch B")) {
		t.Fatal("batch partial outcomes are not visible")
	}
	for _, page := range []string{"/admin/users", "/admin/groups"} {
		response, err := admin.Get(base + page)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !bytes.Contains(body, []byte("</html>")) {
			t.Fatalf("page failed to render completely: %s", page)
		}
	}
	api := func(client *http.Client, method, path, csrf, bearer, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		req.Header.Set("X-CSRF-Token", csrf)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	input, _ := json.Marshal(storage.UserInput{Name: "Injected", UserGroupID: groups[0].GroupID, Enabled: true})
	if status := api(admin, "POST", "/api/admin/users", "", "", string(input)); status != http.StatusForbidden {
		t.Fatalf("missing CSRF=%d", status)
	}
	if status := api(admin, "POST", "/api/admin/users", csrf, "", `{"name":"Injected","user_id":"supplied","user_group_id":"`+groups[0].GroupID+`"}`); status != http.StatusBadRequest {
		t.Fatalf("identity overposting=%d", status)
	}
	bearer, _ := security.NewToken(security.MachineSecretBytes)
	unauthenticated := &http.Client{Transport: h.Client().Transport}
	if status := api(unauthenticated, "GET", "/api/admin/users", "", bearer, ""); status != http.StatusUnauthorized {
		t.Fatalf("machine bearer gained User admin access=%d", status)
	}
	if status := api(admin, "POST", "/api/admin/users/"+user.UserID+"/rotate", csrf, "", ""); status != http.StatusOK {
		t.Fatalf("rotate API=%d", status)
	}
	rotated, err := s.store.GetUser(context.Background(), user.UserID)
	if err != nil || rotated.CredentialUUID == user.CredentialUUID {
		t.Fatal("rotation did not change formal secret", err)
	}
	batch := storage.UserBatchInput{Operation: "disable", UserIDs: []string{user.UserID, "missing-user"}}
	encoded, _ := json.Marshal(batch)
	if status := api(admin, "POST", "/api/admin/users/batch/change", csrf, "", string(encoded)); status != http.StatusOK {
		t.Fatalf("partial batch API=%d", status)
	}
	if access, err := s.store.GetUserAccess(context.Background(), user.UserID, time.Now()); err != nil || access.User.Enabled {
		t.Fatal("batch disable failed", err)
	}
}
