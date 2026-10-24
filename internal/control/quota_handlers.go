package control

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/storage"
)

type quotaValueWire struct {
	QuotaBytes *quantity.Bytes `json:"quota_bytes"`
}

type machineBudgetWire struct {
	BudgetBytes  *quantity.Bytes `json:"budget_bytes"`
	WarningBytes *quantity.Bytes `json:"warning_bytes"`
	HardStop     bool            `json:"hard_stop"`
}

func parseQuotaValue(raw string) (*quantity.Bytes, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := quantity.Parse(raw)
	if err != nil {
		return nil, storage.ErrInvalidQuota
	}
	return &value, nil
}

func parseUserNodeQuotaForm(form map[string][]string) (*quantity.Bytes, bool, error) {
	mode := "inherit"
	if values := form["quota_mode"]; len(values) == 1 {
		mode = values[0]
	}
	switch mode {
	case "inherit":
		return nil, true, nil
	case "unlimited":
		return nil, false, nil
	case "limit":
		value, err := parseQuotaValue(formValue(form, "quota_bytes"))
		if err != nil || value == nil {
			return nil, false, storage.ErrInvalidQuota
		}
		return value, false, nil
	default:
		return nil, false, storage.ErrInvalidQuota
	}
}

func formValue(form map[string][]string, key string) string {
	values := form[key]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func (s *Server) handleUserNodeQuotaForm(w http.ResponseWriter, r *http.Request) {
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	quota, inherit, err := parseUserNodeQuotaForm(form)
	if err == nil {
		if inherit {
			err = s.store.DeleteUserNodeQuota(r.Context(), r.PathValue("userID"), r.PathValue("nodeID"), time.Now())
		} else {
			err = s.store.SaveUserNodeQuota(r.Context(), r.PathValue("userID"), r.PathValue("nodeID"), quota, time.Now())
		}
	}
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (s *Server) handleUserNodeQuotaAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	userID, nodeID := r.PathValue("userID"), r.PathValue("nodeID")
	if r.Method == http.MethodGet {
		record, err := s.store.GetUserNodeQuota(r.Context(), userID, nodeID)
		if err != nil {
			writeQuotaAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user_id": userID, "node_id": nodeID, "exists": record.Exists, "quota_bytes": record.QuotaBytes, "csrf": session.csrf})
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	var err error
	switch r.Method {
	case http.MethodDelete:
		err = s.store.DeleteUserNodeQuota(r.Context(), userID, nodeID, time.Now())
	case http.MethodPut, http.MethodPost:
		var input quotaValueWire
		if err = decodeLimitedJSON(w, r, &input, 8<<10); err == nil {
			err = s.store.SaveUserNodeQuota(r.Context(), userID, nodeID, input.QuotaBytes, time.Now())
		}
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	if err != nil {
		writeQuotaAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

func (s *Server) handleUserQuotaAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	summary, err := s.store.GetUserQuotaSummary(r.Context(), r.PathValue("userID"), time.Now())
	if err != nil {
		writeQuotaAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": summary, "csrf": session.csrf})
}

func (s *Server) handleMachineQuotaForm(w http.ResponseWriter, r *http.Request) {
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	budget, err := machineBudgetFromForm(form)
	if err == nil {
		budget.MachineID = r.PathValue("machineID")
		err = s.store.SaveMachineBudget(r.Context(), budget, time.Now())
	}
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines", http.StatusSeeOther)
}

func machineBudgetFromForm(form map[string][]string) (storage.MachineBudget, error) {
	mode := formValue(form, "budget_mode")
	if mode == "" {
		mode = "limit"
	}
	var budget storage.MachineBudget
	switch mode {
	case "unlimited":
		budget.BudgetBytes = nil
	case "limit":
		value, err := parseQuotaValue(formValue(form, "budget_bytes"))
		if err != nil || value == nil {
			return budget, storage.ErrInvalidQuota
		}
		budget.BudgetBytes = value
	default:
		return budget, storage.ErrInvalidQuota
	}
	warning, err := parseQuotaValue(formValue(form, "warning_bytes"))
	if err != nil {
		return budget, err
	}
	budget.WarningBytes = warning
	budget.HardStop = formValue(form, "hard_stop") == "true"
	return budget, nil
}

func (s *Server) handleMachineQuotaAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	machineID := r.PathValue("machineID")
	if r.Method == http.MethodGet {
		summary, err := s.store.GetMachineQuotaSummary(r.Context(), machineID, time.Now())
		if err != nil {
			writeQuotaAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"quota": summary, "csrf": session.csrf})
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, PUT")
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	var input machineBudgetWire
	if err := decodeLimitedJSON(w, r, &input, 8<<10); err != nil {
		writeJSONError(w, http.StatusBadRequest, "机器额度消息格式无效")
		return
	}
	err := s.store.SaveMachineBudget(r.Context(), storage.MachineBudget{MachineID: machineID, BudgetBytes: input.BudgetBytes, WarningBytes: input.WarningBytes, HardStop: input.HardStop}, time.Now())
	if err != nil {
		writeQuotaAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

func (s *Server) handleQuotaResetForm(w http.ResponseWriter, r *http.Request) {
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	err := s.store.ResetQuotaPeriod(r.Context(), period.Scope{Kind: formValue(form, "kind"), UserID: formValue(form, "user_id"), NodeID: formValue(form, "node_id"), MachineID: formValue(form, "machine_id")}, time.Now().UTC())
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	redirect := formValue(form, "return_to")
	if redirect != "/admin/users" && redirect != "/admin/machines" {
		redirect = "/admin/users"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleQuotaResetAPI(w http.ResponseWriter, r *http.Request) {
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
	var scope period.Scope
	if err := decodeLimitedJSON(w, r, &scope, 8<<10); err != nil || scope.Validate() != nil {
		writeJSONError(w, http.StatusBadRequest, "周期范围无效")
		return
	}
	if err := s.store.ResetQuotaPeriod(r.Context(), scope, time.Now().UTC()); err != nil {
		writeQuotaAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

func writeQuotaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrInvalidQuota):
		http.Error(w, "额度或周期字段无效。", http.StatusBadRequest)
	case errors.Is(err, storage.ErrUserNotFound), errors.Is(err, storage.ErrNodeNotFound), errors.Is(err, storage.ErrMachineNotFound):
		http.Error(w, "用户、节点或机器不存在。", http.StatusNotFound)
	default:
		http.Error(w, "额度操作暂时无法完成。", http.StatusInternalServerError)
	}
}

func writeQuotaAPIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "额度操作暂时无法完成"
	switch {
	case errors.Is(err, storage.ErrInvalidQuota):
		status, message = http.StatusBadRequest, "额度或周期字段无效"
	case errors.Is(err, storage.ErrUserNotFound), errors.Is(err, storage.ErrNodeNotFound), errors.Is(err, storage.ErrMachineNotFound):
		status, message = http.StatusNotFound, "用户、节点或机器不存在"
	}
	writeJSONError(w, status, message)
}
