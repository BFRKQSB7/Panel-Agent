package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
	"panel-agent.local/panel-agent/internal/web"
)

type tokenBatchRequest struct {
	Kind        string                  `json:"kind"`
	ActionToken string                  `json:"action_token"`
	Items       []storage.TokenMutation `json:"items"`
}

type userLinkRow struct {
	ID, Name string
	Selected bool
	State    storage.UserTokenState
}

// Bind actions to authority as well as the stable master key. A restored or
// rotated authority must not accept actions issued by the old authority.
func (s *Server) tokenActionSubject(ctx context.Context, suffix string) (string, error) {
	authority, err := s.store.GetPanelAuthority(ctx)
	return authority.Epoch + ":" + suffix, err
}

func (s *Server) adminTokenAction(ctx context.Context) (string, error) {
	subject, err := s.tokenActionSubject(ctx, "admin")
	if err != nil {
		return "", err
	}
	return s.actions.Issue("admin-user-tokens", subject, time.Now())
}

func (s *Server) handleTokenActionAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	action, err := s.adminTokenAction(r.Context())
	if err != nil {
		writeTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"action_token": action, "csrf": session.csrf})
}

func (s *Server) resetTokenBatch(r *http.Request, input tokenBatchRequest) ([]storage.TokenDelivery, error) {
	if input.Kind != "dashboard" && input.Kind != "subscription" && input.Kind != "both" || len(input.Items) == 0 || len(input.Items) > 100 {
		return nil, storage.ErrInvalidUser
	}
	if input.Kind == "both" {
		for _, item := range input.Items {
			if item.Spec.Value != "" {
				return nil, security.ErrWeakCapability
			}
		}
	}
	subject, err := s.tokenActionSubject(r.Context(), "admin")
	if err != nil {
		return nil, err
	}
	op, err := s.actions.Verify(input.ActionToken, "admin-user-tokens", subject, time.Now())
	if err != nil {
		return nil, errInvalidTokenAction
	}
	kinds := []string{input.Kind}
	if input.Kind == "both" {
		kinds = []string{"dashboard", "subscription"}
	}
	results := []storage.TokenDelivery{}
	for _, kind := range kinds {
		items, err := s.store.ResetUserTokens(r.Context(), op, kind, input.Items, time.Now())
		if err != nil {
			return nil, err
		}
		results = append(results, items...)
	}
	return results, nil
}

var errInvalidTokenAction = errors.New("action token invalid or expired")

func (s *Server) handleTokenBatchAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	var input tokenBatchRequest
	if err := decodeLimitedJSON(w, r, &input, 64<<10); err != nil {
		http.Error(w, "Token 消息格式无效", http.StatusBadRequest)
		return
	}
	results, err := s.resetTokenBatch(r, input)
	if err != nil {
		writeTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleUserLinksPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	action, err := s.adminTokenAction(r.Context())
	if err != nil {
		writeTokenError(w, err)
		return
	}
	s.renderUserLinks(w, r, action, nil, nil)
}

func (s *Server) renderUserLinks(w http.ResponseWriter, r *http.Request, action string, results []storage.TokenDelivery, selected map[string]bool) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	rows := []userLinkRow{}
	for _, user := range users {
		state, err := s.store.GetUserTokenState(r.Context(), user.UserID)
		if err != nil {
			writeTokenError(w, err)
			return
		}
		rows = append(rows, userLinkRow{ID: user.UserID, Name: user.Name, State: state, Selected: selected[user.UserID] || r.URL.Query().Get("user_id") == user.UserID})
	}
	tokenPageHeaders(w)
	_ = s.templates.ExecuteTemplate(w, "user-links.html", map[string]any{"Rows": rows, "Results": results, "ActionToken": action, "CSRF": session.csrf})
}

func (s *Server) handleUserLinksForm(w http.ResponseWriter, r *http.Request) {
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	input := tokenBatchRequest{Kind: form.Get("kind"), ActionToken: form.Get("action_token")}
	selected := map[string]bool{}
	for _, id := range form["user_ids"] {
		input.Items = append(input.Items, storage.TokenMutation{UserID: id, Spec: security.CapabilitySpec{Prefix: form.Get("prefix"), Value: form.Get("value")}})
		selected[id] = true
	}
	results, err := s.resetTokenBatch(r, input)
	if err != nil {
		writeTokenError(w, err)
		return
	}
	s.renderUserLinks(w, r, input.ActionToken, results, selected)
}

func writeTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidTokenAction):
		http.Error(w, "操作凭据无效或已过期，请重新打开页面。", http.StatusForbidden)
	case errors.Is(err, security.ErrWeakCapability):
		http.Error(w, "Token 格式或强度不足。完整自定义值仅限单类 Token，不能同时填写前缀。", http.StatusBadRequest)
	case errors.Is(err, storage.ErrCapabilityConflict):
		http.Error(w, "Token 已使用，请选择新值。", http.StatusConflict)
	default:
		writeDomainError(w, err)
	}
}

func tokenPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
}

func (s *Server) handleTokenScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(web.TokenScript())
}
