package control

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

func (s *Server) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	machine, err := s.store.GetMachine(r.Context(), r.PathValue("machineID"))
	if err != nil {
		writeNodeError(w, err)
		return
	}
	nodes, err := s.store.ListNodes(r.Context(), machine.MachineID)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "nodes.html", map[string]any{
		"CSRF": session.csrf, "Machine": storage.MachineViewFromRecord(machine), "Nodes": nodes,
		"NewNode": storage.NodeInput{Enabled: true, ListenHost: "0.0.0.0", ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"},
	})
}

func (s *Server) handleNodeForm(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	publicPort, _ := strconv.Atoi(form.Get("public_port"))
	listenPort, _ := strconv.Atoi(form.Get("listen_port"))
	_, err = s.store.SaveNode(r.Context(), r.PathValue("machineID"), r.PathValue("nodeID"), storage.NodeInput{
		Name: form.Get("name"), PublicHost: form.Get("public_host"), PublicPort: publicPort,
		ListenHost: form.Get("listen_host"), ListenPort: listenPort, Enabled: form.Get("enabled") == "true",
		ServerName: form.Get("server_name"), Target: form.Get("target"), Development: form.Get("development") == "true",
	}, time.Now())
	if err != nil {
		writeNodeError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines/"+url.PathEscape(r.PathValue("machineID"))+"/nodes", http.StatusSeeOther)
}

func (s *Server) handleNodeDeleteForm(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readForm(w, r)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if err := s.store.DeleteNode(r.Context(), r.PathValue("machineID"), r.PathValue("nodeID"), time.Now()); err != nil {
		writeNodeError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines/"+url.PathEscape(r.PathValue("machineID"))+"/nodes", http.StatusSeeOther)
}

func (s *Server) handleNodesAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	machineID, nodeID := r.PathValue("machineID"), r.PathValue("nodeID")
	if r.Method == http.MethodGet {
		nodes, err := s.store.ListNodes(r.Context(), machineID)
		if err != nil {
			writeNodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "csrf": session.csrf})
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeJSONError(w, http.StatusForbidden, "请求校验失败")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteNode(r.Context(), machineID, nodeID, time.Now()); err != nil {
			writeNodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	var input storage.NodeInput
	if err := decodeProtocolJSON(w, r, &input); err != nil {
		writeProtocolError(w, http.StatusBadRequest, protocol.ErrorInvalidMessage, "节点消息格式无效")
		return
	}
	node, err := s.store.SaveNode(r.Context(), machineID, nodeID, input, time.Now())
	if err != nil {
		writeNodeError(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, node)
}

func writeNodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrMachineNotFound), errors.Is(err, storage.ErrNodeNotFound):
		http.Error(w, "机器或节点不存在", http.StatusNotFound)
	case errors.Is(err, storage.ErrInvalidNode):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, storage.ErrNodePortConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, storage.ErrMachineAuthentication):
		http.Error(w, "机器凭据已吊销，请重新配对后再变更节点。", http.StatusConflict)
	default:
		http.Error(w, "节点操作暂时无法完成。", http.StatusInternalServerError)
	}
}
