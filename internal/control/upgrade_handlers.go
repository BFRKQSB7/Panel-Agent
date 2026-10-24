package control

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

type upgradeIntentRequest struct {
	TargetVersion string `json:"target_version"`
	SHA256        string `json:"sha256"`
	PackageSize   uint64 `json:"package_size,string"`
}

func (s *Server) handleMachineUpgradeForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "不支持此请求方法", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readFormWithLimit(w, r, 16<<10)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	request, err := parseUpgradeForm(form)
	if err != nil {
		http.Error(w, "升级 Intent 无效："+err.Error(), http.StatusBadRequest)
		return
	}
	intent, err := s.store.CreateUpgradeIntent(r.Context(), storage.UpgradeIntentInput{MachineID: r.PathValue("machineID"), TargetVersion: request.TargetVersion, Source: protocol.UpgradeSourceOfficialXray, SHA256: request.SHA256, PackageSize: request.PackageSize}, time.Now().UTC())
	if err != nil {
		writeUpgradeIntentError(w, err, false)
		return
	}
	http.Redirect(w, r, "/admin/machines?upgrade="+intent.ID, http.StatusSeeOther)
}

func (s *Server) handleMachineUpgradeAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持此请求方法")
		return
	}
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	var request upgradeIntentRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "升级 Intent 格式无效")
		return
	}
	intent, err := s.store.CreateUpgradeIntent(r.Context(), storage.UpgradeIntentInput{MachineID: r.PathValue("machineID"), TargetVersion: request.TargetVersion, Source: protocol.UpgradeSourceOfficialXray, SHA256: request.SHA256, PackageSize: request.PackageSize}, time.Now().UTC())
	if err != nil {
		writeUpgradeIntentError(w, err, true)
		return
	}
	writeJSON(w, http.StatusCreated, intent)
}

func (s *Server) handleAgentUpgradeIntents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var request protocol.UpgradePullRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil || request.Version != protocol.UpgradeProtocolVersion || !validUUIDv4(request.RequestID) || !validUUIDv4(request.AuthorityEpoch) {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "升级 Intent 请求无效")
		return
	}
	intents, err := s.store.PullUpgradeIntents(r.Context(), machine.MachineID, request.AuthorityEpoch, time.Now().UTC())
	if err != nil {
		if errors.Is(err, storage.ErrUpgradeIntentStale) {
			writeProtocolError(w, http.StatusConflict, protocol.ErrorAuthorityMismatch, "升级 Intent authority 已变化")
			return
		}
		if errors.Is(err, storage.ErrMachineAuthentication) {
			writeProtocolError(w, http.StatusUnauthorized, protocol.ErrorAuthenticationRequired, "机器认证失败")
			return
		}
		writeProtocolError(w, http.StatusInternalServerError, protocol.ErrorInvalidMessage, "无法读取升级 Intent")
		return
	}
	writeJSON(w, http.StatusOK, protocol.UpgradePullResponse{Version: protocol.UpgradeProtocolVersion, RequestID: request.RequestID, AuthorityEpoch: request.AuthorityEpoch, Intents: intents})
}

func (s *Server) handleAgentUpgradeResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, protocol.ErrorInvalidMessage, "仅支持 POST 请求")
		return
	}
	machine, ok := s.authenticatedAgent(w, r)
	if !ok {
		return
	}
	var request protocol.UpgradeResultRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil || request.Version != protocol.UpgradeProtocolVersion || !validUUIDv4(request.RequestID) || !validUUIDv4(request.IntentID) || !validUUIDv4(request.AuthorityEpoch) {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "升级结果格式无效")
		return
	}
	if err := s.store.RecordUpgradeResult(r.Context(), machine.MachineID, request, time.Now().UTC()); err != nil {
		if errors.Is(err, storage.ErrUpgradeIntentStale) {
			writeProtocolError(w, http.StatusConflict, protocol.ErrorAuthorityMismatch, "升级结果 authority 已变化")
			return
		}
		if errors.Is(err, storage.ErrUpgradeIntentNotFound) || errors.Is(err, storage.ErrUpgradeIntentTerminal) {
			writeProtocolError(w, http.StatusConflict, protocol.ErrorInvalidMessage, "升级 Intent 已变化或不存在")
			return
		}
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "升级结果无效")
		return
	}
	writeJSON(w, http.StatusOK, protocol.UpgradeResultResponse{Version: protocol.UpgradeProtocolVersion, RequestID: request.RequestID, Accepted: true})
}

func parseUpgradeForm(form map[string][]string) (upgradeIntentRequest, error) {
	value := func(name string) string {
		if values := form[name]; len(values) != 0 {
			return strings.TrimSpace(values[0])
		}
		return ""
	}
	size, err := strconv.ParseUint(value("package_size"), 10, 64)
	if err != nil {
		return upgradeIntentRequest{}, fmt.Errorf("package_size 无效")
	}
	return upgradeIntentRequest{TargetVersion: value("target_version"), SHA256: value("sha256"), PackageSize: size}, nil
}

func writeUpgradeIntentError(w http.ResponseWriter, err error, api bool) {
	status := http.StatusInternalServerError
	if errors.Is(err, storage.ErrUpgradeIntentInvalid) {
		status = http.StatusBadRequest
	} else if errors.Is(err, storage.ErrUpgradeIntentStale) {
		status = http.StatusConflict
	} else if errors.Is(err, storage.ErrMachineNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, storage.ErrMachineAuthentication) {
		status = http.StatusUnauthorized
	}
	if api {
		writeJSONError(w, status, err.Error())
	} else {
		http.Error(w, "无法创建升级 Intent："+err.Error(), status)
	}
}
