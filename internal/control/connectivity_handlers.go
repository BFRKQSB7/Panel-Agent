package control

import (
	"errors"
	"net/http"

	"panel-agent.local/panel-agent/internal/storage"
	"panel-agent.local/panel-agent/internal/web"
)

type connectivityRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleConnectivityScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(web.ConnectivityScript())
}

func (s *Server) handleNodeConnectivityCheck(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	var request connectivityRequest
	if err := decodeProtocolJSON(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "探测请求格式无效")
		return
	}
	if request.Mode == "" {
		request.Mode = connectivityModePublic
	}
	if request.Mode != connectivityModePublic && request.Mode != connectivityModeLocal {
		writeJSONError(w, http.StatusBadRequest, "不支持的探测模式")
		return
	}
	select {
	case s.probeLimiter <- struct{}{}:
		defer func() { <-s.probeLimiter }()
	default:
		writeJSONError(w, http.StatusTooManyRequests, "探测并发已达上限，请稍后重试")
		return
	}
	nodes, err := s.store.ListNodes(r.Context(), r.PathValue("machineID"))
	if err != nil {
		if errors.Is(err, storage.ErrMachineNotFound) {
			writeJSONError(w, http.StatusNotFound, "机器不存在")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "节点暂时不可用")
		}
		return
	}
	var node storage.NodeRecord
	found := false
	for _, candidate := range nodes {
		if candidate.NodeID == r.PathValue("nodeID") {
			node, found = candidate, true
			break
		}
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "节点不存在")
		return
	}
	result := runConnectivityProbe(r.Context(), node, request.Mode)
	if result.DNS.Status == "rejected" {
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
