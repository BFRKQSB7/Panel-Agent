package control

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/extensions"
	"panel-agent.local/panel-agent/internal/storage"
)

const maxExtensionUploadBytes = 2 << 20

func (s *Server) handleExtensionsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	items, err := s.store.ListExtensions(r.Context())
	if err != nil {
		http.Error(w, "扩展列表暂时不可用。", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "extensions.html", map[string]any{"CSRF": session.csrf, "Extensions": items})
}

func (s *Server) handleExtensionImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExtensionUploadBytes)
	if err := r.ParseMultipartForm(maxExtensionUploadBytes); err != nil {
		http.Error(w, "扩展包请求超限或格式无效。", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !s.csrfValid(r, session.csrf, r.FormValue("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	file, _, err := r.FormFile("package")
	if err != nil {
		http.Error(w, "请选择扩展 ZIP 包。", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, extensions.MaxCompressedBytes+1))
	if err != nil || len(data) > extensions.MaxCompressedBytes {
		http.Error(w, "扩展包大小超限。", http.StatusBadRequest)
		return
	}
	pkg, err := extensions.ImportZip(data)
	if err != nil {
		http.Error(w, "扩展包未通过静态验证。", http.StatusBadRequest)
		return
	}
	if _, err := s.store.InstallExtension(r.Context(), pkg, time.Now()); err != nil {
		writeExtensionError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/extensions", http.StatusSeeOther)
}

func (s *Server) handleExtensionEnable(w http.ResponseWriter, r *http.Request) {
	s.handleExtensionState(w, r, true)
}

func (s *Server) handleExtensionDisable(w http.ResponseWriter, r *http.Request) {
	s.handleExtensionState(w, r, false)
}

func (s *Server) handleExtensionState(w http.ResponseWriter, r *http.Request, enabled bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	err = s.store.SetExtensionEnabled(r.Context(), r.PathValue("extensionID"), r.PathValue("version"), enabled, time.Now())
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/extensions", http.StatusSeeOther)
}

func (s *Server) handleExtensionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if err := s.store.DeleteExtension(r.Context(), r.PathValue("extensionID"), r.PathValue("version")); err != nil {
		writeExtensionError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/extensions", http.StatusSeeOther)
}

func writeExtensionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrExtensionNotFound):
		http.Error(w, "扩展版本不存在。", http.StatusNotFound)
	case errors.Is(err, storage.ErrExtensionReferenced):
		http.Error(w, "扩展版本仍被节点引用，请先解除节点引用。", http.StatusConflict)
	case errors.Is(err, storage.ErrExtensionExists):
		http.Error(w, "相同扩展版本已经导入。", http.StatusConflict)
	case errors.Is(err, storage.ErrExtensionDisabled):
		http.Error(w, "扩展版本已停用，不能新建依赖。", http.StatusConflict)
	default:
		http.Error(w, fmt.Sprintf("扩展操作失败：%s", safeExtensionError(err)), http.StatusBadRequest)
	}
}

func safeExtensionError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len([]rune(message)) > 160 {
		return string([]rune(message)[:160])
	}
	return message
}
