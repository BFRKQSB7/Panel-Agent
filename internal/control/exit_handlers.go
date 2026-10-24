package control

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

func exitInputFromForm(form interface{ Get(string) string }) (storage.ExitInput, error) {
	publicPort, err := strconv.Atoi(form.Get("public_port"))
	if err != nil {
		return storage.ExitInput{}, storage.ErrInvalidExit
	}
	listenPort, err := strconv.Atoi(form.Get("listen_port"))
	if err != nil {
		return storage.ExitInput{}, storage.ErrInvalidExit
	}
	return storage.ExitInput{PublicHost: form.Get("public_host"), PublicPort: publicPort, ListenHost: form.Get("listen_host"), ListenPort: listenPort, ServerName: form.Get("server_name"), Target: form.Get("target")}, nil
}

func (s *Server) handleExitForm(w http.ResponseWriter, r *http.Request) {
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	input, err := exitInputFromForm(form)
	if err == nil {
		_, err = s.store.SaveExit(r.Context(), r.PathValue("machineID"), input, time.Now())
	}
	if err != nil {
		writeExitError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) handleExitDeleteForm(w http.ResponseWriter, r *http.Request) {
	_, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteExit(r.Context(), r.PathValue("machineID"), time.Now()); err != nil {
		writeExitError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) handleExitRotateForm(w http.ResponseWriter, r *http.Request) {
	_, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	if err := s.store.RotateExitTransit(r.Context(), r.PathValue("machineID"), time.Now()); err != nil {
		writeExitError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func (s *Server) handleExitAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	machineID := r.PathValue("machineID")
	if r.Method == http.MethodGet {
		exit, err := s.store.GetExit(r.Context(), machineID)
		if err != nil {
			writeExitAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"exit": exit, "csrf": session.csrf})
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var input storage.ExitInput
		if err := decodeProtocolJSON(w, r, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Exit 消息格式无效")
			return
		}
		if _, err := s.store.SaveExit(r.Context(), machineID, input, time.Now()); err != nil {
			writeExitAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
	case http.MethodDelete:
		if err := s.store.DeleteExit(r.Context(), machineID, time.Now()); err != nil {
			writeExitAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
	}
}

func (s *Server) handleExitRotateAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	if err := s.store.RotateExitTransit(r.Context(), r.PathValue("machineID"), time.Now()); err != nil {
		writeExitAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"rotated": true})
}

func writeExitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrInvalidExit):
		http.Error(w, "Exit 配置无效。", http.StatusBadRequest)
	case errors.Is(err, storage.ErrExitNotFound), errors.Is(err, storage.ErrMachineNotFound):
		http.Error(w, "Exit 或机器不存在。", http.StatusNotFound)
	case errors.Is(err, storage.ErrExitInUse), errors.Is(err, storage.ErrNodePortConflict):
		http.Error(w, "Exit 仍被使用或端口冲突。", http.StatusConflict)
	case errors.Is(err, storage.ErrMachineAuthentication):
		http.Error(w, "机器凭据已吊销，请重新配对后再变更 Exit。", http.StatusConflict)
	default:
		http.Error(w, "Exit 操作暂时无法完成。", http.StatusInternalServerError)
	}
}

func writeExitAPIError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "Exit 操作暂时无法完成"
	switch {
	case errors.Is(err, storage.ErrInvalidExit):
		status, message = http.StatusBadRequest, "Exit 配置无效"
	case errors.Is(err, storage.ErrExitNotFound), errors.Is(err, storage.ErrMachineNotFound):
		status, message = http.StatusNotFound, "Exit 或机器不存在"
	case errors.Is(err, storage.ErrExitInUse), errors.Is(err, storage.ErrNodePortConflict):
		status, message = http.StatusConflict, "Exit 仍被使用或端口冲突"
	case errors.Is(err, storage.ErrMachineAuthentication):
		status, message = http.StatusConflict, "机器凭据已吊销"
	}
	writeJSONError(w, status, message)
}
