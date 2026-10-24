package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	exits, err := s.store.ListExitOptions(r.Context(), machine.MachineID)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	extensions, err := s.store.ListExtensions(r.Context())
	if err != nil {
		http.Error(w, "协议扩展暂时不可用。", http.StatusInternalServerError)
		return
	}
	guides := make(map[string]nodeGuide, len(nodes))
	machineView := storage.MachineViewFromRecord(machine)
	for _, node := range nodes {
		guides[node.NodeID] = makeNodeGuide(machine.MachineID, node, machineView)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "nodes.html", map[string]any{
		"CSRF": session.csrf, "Machine": machineView, "Nodes": nodes, "Exits": exits, "Guides": guides, "Extensions": extensions,
		"NewNode": storage.NodeInput{Enabled: true, ListenHost: "0.0.0.0", ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443"},
	})
}

func (s *Server) handleNodeForm(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	form, err := readFormWithLimit(w, r, 64<<10)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	publicPort, _ := strconv.Atoi(form.Get("public_port"))
	listenPort, _ := strconv.Atoi(form.Get("listen_port"))
	quota, err := parseQuotaValue(form.Get("quota_bytes"))
	if err != nil {
		writeNodeError(w, err)
		return
	}
	assignment, err := s.nodeExtensionAssignment(r.Context(), form)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	_, err = s.store.SaveNodeWithExtension(r.Context(), r.PathValue("machineID"), r.PathValue("nodeID"), storage.NodeInput{
		Name: form.Get("name"), PublicHost: form.Get("public_host"), PublicPort: publicPort,
		ListenHost: form.Get("listen_host"), ListenPort: listenPort, Enabled: form.Get("enabled") == "true",
		ServerName: form.Get("server_name"), Target: form.Get("target"), Development: form.Get("development") == "true", QuotaBytes: quota,
		ExitMachineID: form.Get("exit_machine_id"),
	}, assignment, time.Now())
	if err != nil {
		writeNodeError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/machines/"+url.PathEscape(r.PathValue("machineID"))+"/nodes", http.StatusSeeOther)
}

func (s *Server) nodeExtensionAssignment(ctx context.Context, form url.Values) (*storage.NodeExtensionInput, error) {
	ref := strings.TrimSpace(form.Get("extension_id"))
	if ref == "" {
		return nil, nil
	}
	id, version, ok := strings.Cut(ref, "@")
	if !ok || id == "" || version == "" {
		return nil, storage.ErrInvalidExtension
	}
	record, err := s.store.GetExtension(ctx, id, version)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(record.Package.Schema.Fields))
	for _, field := range record.Package.Schema.Fields {
		values[field.Name] = form.Get("extension_" + id + "_" + version + "_" + field.Name)
	}
	return &storage.NodeExtensionInput{ID: id, Version: version, Values: values}, nil
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
	case errors.Is(err, storage.ErrInvalidQuota):
		http.Error(w, "节点额度无效", http.StatusBadRequest)
	case errors.Is(err, storage.ErrInvalidExit):
		http.Error(w, "出口选择无效", http.StatusBadRequest)
	case errors.Is(err, storage.ErrExitNotFound):
		http.Error(w, "出口不存在或尚未配置", http.StatusConflict)
	case errors.Is(err, storage.ErrNodePortConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, storage.ErrMachineAuthentication):
		http.Error(w, "机器凭据已吊销，请重新配对后再变更节点。", http.StatusConflict)
	case errors.Is(err, storage.ErrExtensionNotFound):
		http.Error(w, "协议扩展不存在。", http.StatusNotFound)
	case errors.Is(err, storage.ErrExtensionDisabled):
		http.Error(w, "协议扩展已停用，不能新建依赖。", http.StatusConflict)
	case errors.Is(err, storage.ErrExtensionReferenced), errors.Is(err, storage.ErrInvalidExtension):
		http.Error(w, "协议扩展配置无效或仍被引用。", http.StatusBadRequest)
	default:
		http.Error(w, "节点操作暂时无法完成。", http.StatusInternalServerError)
	}
}
