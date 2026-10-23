package control

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

const (
	sessionCookieName = "panel_session"
	csrfCookieName    = "panel_csrf"
	maxFormBytes      = 8 << 10
)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requestSession(r); ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.requestSession(r); ok {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		s.renderLogin(w, http.StatusOK, "")
	case http.MethodPost:
		s.login(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	form, err := readForm(w, r)
	if err != nil {
		s.renderLogin(w, http.StatusBadRequest, "请求无法处理。")
		return
	}
	csrf, err := r.Cookie(csrfCookieName)
	if err != nil || !s.csrfValid(r, csrf.Value, form.Get("csrf")) {
		s.renderLogin(w, http.StatusForbidden, "请求校验失败。")
		return
	}

	clientKey := remoteClientKey(r.RemoteAddr)
	now := time.Now()
	if allowed, wait := s.limiter.Allowed(clientKey, now); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
		s.renderLogin(w, http.StatusTooManyRequests, "登录尝试次数过多，请稍后再试。")
		return
	}
	password := []byte(form.Get("password"))
	defer clear(password)
	valid, err := s.store.VerifyAdmin(r.Context(), func(hash string) bool {
		return security.VerifyAdminPassword(hash, password)
	})
	if err != nil {
		http.Error(w, "认证服务暂时不可用。", http.StatusInternalServerError)
		return
	}
	if !valid {
		wait := s.limiter.Failed(clientKey, now)
		if wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
			s.renderLogin(w, http.StatusTooManyRequests, "登录尝试次数过多，请稍后再试。")
			return
		}
		s.renderLogin(w, http.StatusUnauthorized, "管理员口令错误。")
		return
	}
	s.limiter.Succeeded(clientKey)
	token, session, err := s.sessions.Create(now)
	if err != nil {
		http.Error(w, "无法创建登录会话。", http.StatusInternalServerError)
		return
	}
	s.setSessionCookies(w, token, session)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, message string) {
	token, err := randomCSRFToken()
	if err != nil {
		http.Error(w, "登录页面暂时不可用。", http.StatusInternalServerError)
		return
	}
	s.setCSRFCookie(w, token, time.Now().Add(15*time.Minute))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, "login.html", map[string]string{"CSRF": token, "Error": message}); err != nil {
		return
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, token, ok := s.requestSessionWithToken(r)
	if !ok {
		http.Error(w, "尚未登录", http.StatusUnauthorized)
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	s.sessions.Delete(token)
	s.clearSessionCookies(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requestSession(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, "设置暂时不可用。", http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, settings.SiteName, session.csrf, "", r.URL.Query().Get("saved") == "1", http.StatusOK)
}

func (s *Server) handleSettingsForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requestSession(r)
	if !ok {
		http.Error(w, "尚未登录", http.StatusUnauthorized)
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	name := form.Get("site_name")
	if err := validateSiteName(name); err != nil {
		s.renderSettings(w, name, session.csrf, "面板名称需为 1 至 64 个可见字符。", false, http.StatusBadRequest)
		return
	}
	if err := s.store.SaveSettings(r.Context(), storage.Settings{SiteName: strings.TrimSpace(name)}); err != nil {
		http.Error(w, "无法保存设置。", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?saved=1", http.StatusSeeOther)
}

func (s *Server) renderSettings(w http.ResponseWriter, name, csrf, message string, saved bool, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := map[string]any{"SiteName": name, "CSRF": csrf, "Error": message, "Saved": saved}
	_ = s.templates.ExecuteTemplate(w, "settings.html", data)
}

func (s *Server) handleSettingsAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requestSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "尚未登录")
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.store.GetSettings(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "设置暂时不可用")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"site_name": settings.SiteName})
	case http.MethodPut:
		if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
			writeJSONError(w, http.StatusForbidden, "请求校验失败")
			return
		}
		name, err := decodeSettings(w, r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "设置请求格式无效")
			return
		}
		if err := validateSiteName(name); err != nil {
			writeJSONError(w, http.StatusBadRequest, "面板名称无效")
			return
		}
		settings := storage.Settings{SiteName: strings.TrimSpace(name)}
		if err := s.store.SaveSettings(r.Context(), settings); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "无法保存设置")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"site_name": settings.SiteName})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
	}
}

func (s *Server) requestSession(r *http.Request) (sessionRecord, bool) {
	session, _, ok := s.requestSessionWithToken(r)
	return session, ok
}

func (s *Server) requestSessionWithToken(r *http.Request) (sessionRecord, string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return sessionRecord{}, "", false
	}
	session, ok := s.sessions.Get(cookie.Value, time.Now())
	if !ok {
		return sessionRecord{}, "", false
	}
	return session, cookie.Value, true
}

func (s *Server) csrfValid(r *http.Request, cookieToken, submitted string) bool {
	if cookieToken == "" || submitted == "" || subtle.ConstantTimeCompare([]byte(cookieToken), []byte(submitted)) != 1 {
		return false
	}
	return s.sameOrigin(r)
}

func (s *Server) sameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	return strings.EqualFold(parsed.Scheme, s.requestScheme(r)) && strings.EqualFold(parsed.Host, r.Host)
}

func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.config.TrustProxy && isLoopbackRemote(r.RemoteAddr) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return "https"
	}
	return "http"
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "private, no-store")
		if s.requestScheme(r) == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSessionCookies(w http.ResponseWriter, token string, session sessionRecord) {
	expires := session.expiresAt
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: s.config.secureCookies(),
		SameSite: http.SameSiteStrictMode,
	})
	s.setCSRFCookie(w, session.csrf, expires)
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: token, Path: "/", Expires: expires,
		MaxAge: max(1, int(time.Until(expires).Seconds())), HttpOnly: true, Secure: s.config.secureCookies(),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
			HttpOnly: true, Secure: s.config.secureCookies(), SameSite: http.SameSiteStrictMode,
		})
	}
}

func readForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	return readFormWithLimit(w, r, maxFormBytes)
}

func readFormWithLimit(w http.ResponseWriter, r *http.Request, limit int64) (url.Values, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return nil, errors.New("form content type required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.PostForm, nil
}

func remoteClientKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return host
	}
	return remoteAddr
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateSiteName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len([]rune(trimmed)) > 64 {
		return errors.New("invalid site name")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return errors.New("invalid site name")
		}
	}
	return nil
}

func decodeSettings(w http.ResponseWriter, r *http.Request) (string, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", errors.New("JSON content type required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	decoder := json.NewDecoder(r.Body)
	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("multiple JSON values")
	}
	if len(payload) != 1 || payload["site_name"] == nil {
		return "", errors.New("site_name is required")
	}
	var name string
	if err := json.Unmarshal(payload["site_name"], &name); err != nil {
		return "", err
	}
	return name, nil
}

func randomCSRFToken() (string, error) {
	return newRandomToken()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
