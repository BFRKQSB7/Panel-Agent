package control

import (
	"net/http"
	"net/url"
	"panel-agent.local/panel-agent/internal/quantity"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

type userEditor struct {
	User                          storage.UserRecord
	GroupChoices                  []groupChoice
	Nodes                         []storage.NodeRecord
	Quota                         storage.UserQuotaSummary
	QuotaStatus                   string
	Reasons                       []string
	CSRF, QuotaValue, ExpiryValue string
}

func userInputForm(form url.Values) (storage.UserInput, error) {
	input := storage.UserInput{Name: form.Get("name"), UserGroupID: form.Get("user_group_id"), Enabled: form.Get("enabled") == "true", ExpiresAt: form.Get("expires_at")}
	if input.ExpiresAt != "" {
		if local, err := time.Parse("2006-01-02T15:04", input.ExpiresAt); err == nil {
			input.ExpiresAt = local.UTC().Format(time.RFC3339)
		} else if local, err := time.Parse("2006-01-02T15:04:05", input.ExpiresAt); err == nil {
			input.ExpiresAt = local.UTC().Format(time.RFC3339)
		}
	}
	if value := strings.TrimSpace(form.Get("quota_bytes")); value != "" {
		quota, err := quantity.Parse(value)
		if err != nil || quota < 0 {
			return input, storage.ErrInvalidUser
		}
		input.QuotaBytes = &quota
	}
	return input, nil
}

func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request) {
	s.renderUsersPage(w, r, nil)
}

func (s *Server) renderUsersPage(w http.ResponseWriter, r *http.Request, results []storage.BatchUserResult) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	groups, err := s.store.ListGroups(r.Context(), "user")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	makeEditor := func(user storage.UserRecord) userEditor {
		editor := userEditor{User: user, CSRF: session.csrf, Nodes: []storage.NodeRecord{}, Reasons: []string{}}
		if user.GroupDeleted {
			editor.GroupChoices = append(editor.GroupChoices, groupChoice{ID: user.UserGroupID, Name: user.GroupName + "（已删除，请重新选择）", Selected: true, Disabled: true})
		}
		for _, group := range groups {
			editor.GroupChoices = append(editor.GroupChoices, groupChoice{ID: group.GroupID, Name: group.Name, Selected: group.GroupID == user.UserGroupID})
		}
		if user.QuotaBytes != nil {
			editor.QuotaValue = user.QuotaBytes.String()
		}
		if expiry, err := time.Parse(time.RFC3339, user.ExpiresAt); err == nil {
			editor.ExpiryValue = expiry.UTC().Format("2006-01-02T15:04:05")
		}
		return editor
	}
	rows := []userEditor{}
	now := time.Now()
	for _, user := range users {
		access, err := s.store.GetUserAccess(r.Context(), user.UserID, now)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		quota, err := s.store.GetUserQuotaSummary(r.Context(), user.UserID, now)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		editor := makeEditor(access.User)
		editor.Nodes = access.Nodes
		editor.Quota = quota
		editor.QuotaStatus = quotaUsageStatus(quota.Global.Status, quota.Global.PendingReconcile)
		for _, reason := range access.UnavailableReasons {
			switch reason {
			case "manually_disabled":
				editor.Reasons = append(editor.Reasons, "手动停用")
			case "expired":
				editor.Reasons = append(editor.Reasons, "已到期")
			case "group_deleted":
				editor.Reasons = append(editor.Reasons, "用户组已删除")
			}
		}
		rows = append(rows, editor)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "users.html", map[string]any{"Rows": rows, "New": makeEditor(storage.UserRecord{UserInput: storage.UserInput{Enabled: true}}), "CSRF": session.csrf, "Results": results, "Groups": groups})
}

func (s *Server) handleUserForm(w http.ResponseWriter, r *http.Request) {
	if action := r.PathValue("action"); action != "" && action != "delete" && action != "rotate" {
		http.NotFound(w, r)
		return
	}
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	id := r.PathValue("userID")
	var err error
	switch r.PathValue("action") {
	case "delete":
		err = s.store.DeleteUser(r.Context(), id, time.Now())
	case "rotate":
		_, err = s.store.RotateUserCredential(r.Context(), id, time.Now())
	default:
		var input storage.UserInput
		input, err = userInputForm(form)
		if err == nil {
			_, err = s.store.SaveUser(r.Context(), id, input, time.Now())
		}
	}
	if err != nil {
		writeDomainError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (s *Server) handleUserBatchForm(w http.ResponseWriter, r *http.Request) {
	if operation := r.PathValue("operation"); operation != "create" && operation != "change" {
		http.NotFound(w, r)
		return
	}
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	input, err := userInputForm(form)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	var results []storage.BatchUserResult
	if r.PathValue("operation") == "create" {
		inputs := []storage.UserInput{}
		for _, line := range strings.Split(strings.ReplaceAll(form.Get("names"), "\r\n", "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				item := input
				item.Name = line
				inputs = append(inputs, item)
			}
		}
		results, err = s.store.CreateUsers(r.Context(), inputs, time.Now())
	} else {
		results, err = s.store.ChangeUsers(r.Context(), storage.UserBatchInput{Operation: form.Get("operation"), UserIDs: form["user_ids"], UserGroupID: input.UserGroupID, ExpiresAt: input.ExpiresAt, QuotaBytes: input.QuotaBytes}, time.Now())
	}
	if err != nil {
		writeDomainError(w, err)
		return
	}
	s.renderUsersPage(w, r, results)
}

func (s *Server) handleUsersAPI(w http.ResponseWriter, r *http.Request) {
	if action := r.PathValue("action"); action != "" && action != "rotate" {
		http.NotFound(w, r)
		return
	}
	if batch := r.PathValue("batch"); batch != "" && batch != "create" && batch != "change" {
		http.NotFound(w, r)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("userID")
	if r.Method == http.MethodGet {
		if id != "" {
			access, err := s.store.GetUserAccess(r.Context(), id, time.Now())
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, access)
		} else {
			users, err := s.store.ListUsers(r.Context())
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"users": users, "csrf": session.csrf})
		}
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteUser(r.Context(), id, time.Now()); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	if r.PathValue("action") == "rotate" {
		user, err := s.store.RotateUserCredential(r.Context(), id, time.Now())
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, user)
		return
	}
	if r.PathValue("batch") == "create" {
		var input struct {
			Users []storage.UserInput `json:"users"`
		}
		if err := decodeLimitedJSON(w, r, &input, 64<<10); err != nil {
			http.Error(w, "批量用户消息无效", http.StatusBadRequest)
			return
		}
		results, err := s.store.CreateUsers(r.Context(), input.Users, time.Now())
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	if r.PathValue("batch") == "change" {
		var input storage.UserBatchInput
		if err := decodeLimitedJSON(w, r, &input, 64<<10); err != nil {
			http.Error(w, "批量修改消息无效", http.StatusBadRequest)
			return
		}
		results, err := s.store.ChangeUsers(r.Context(), input, time.Now())
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	var input storage.UserInput
	if err := decodeLimitedJSON(w, r, &input, 64<<10); err != nil {
		http.Error(w, "用户消息无效", http.StatusBadRequest)
		return
	}
	user, err := s.store.SaveUser(r.Context(), id, input, time.Now())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, user)
}
