package control

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

type groupChoice struct {
	ID, Name           string
	Selected, Disabled bool
}
type groupEditor struct {
	Group   storage.GroupRecord
	Kind    string
	CSRF    string
	Choices []groupChoice
}

func (s *Server) handleGroupsPage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	nodeGroups, err := s.store.ListGroups(r.Context(), "node")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	userGroups, err := s.store.ListGroups(r.Context(), "user")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	machines, err := s.store.ListMachines(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nodeChoices := []groupChoice{}
	for _, machine := range machines {
		nodes, err := s.store.ListNodes(r.Context(), machine.MachineID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		for _, node := range nodes {
			label := machine.DisplayName + " / " + node.Name
			if !node.Enabled {
				label += "（已停用）"
			}
			nodeChoices = append(nodeChoices, groupChoice{ID: node.NodeID, Name: label})
		}
	}
	ngChoices := []groupChoice{}
	for _, group := range nodeGroups {
		ngChoices = append(ngChoices, groupChoice{ID: group.GroupID, Name: group.Name})
	}
	editors := func(kind string, groups []storage.GroupRecord, choices []groupChoice) []groupEditor {
		result := []groupEditor{}
		for _, group := range groups {
			members := group.NodeIDs
			if kind == "user" {
				members = group.NodeGroupIDs
			}
			copyChoices := append([]groupChoice{}, choices...)
			for i := range copyChoices {
				for _, id := range members {
					copyChoices[i].Selected = copyChoices[i].Selected || copyChoices[i].ID == id
				}
			}
			result = append(result, groupEditor{Group: group, Kind: kind, CSRF: session.csrf, Choices: copyChoices})
		}
		return result
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "groups.html", map[string]any{"NodeGroups": editors("node", nodeGroups, nodeChoices), "UserGroups": editors("user", userGroups, ngChoices), "NewNodeGroup": groupEditor{Kind: "node", CSRF: session.csrf, Choices: nodeChoices}, "NewUserGroup": groupEditor{Kind: "user", CSRF: session.csrf, Choices: ngChoices}, "CSRF": session.csrf})
}

func (s *Server) domainForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return nil, false
	}
	form, err := readFormWithLimit(w, r, 64<<10)
	if err != nil || !s.csrfValid(r, session.csrf, form.Get("csrf")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return nil, false
	}
	return form, true
}

func (s *Server) handleGroupForm(w http.ResponseWriter, r *http.Request) {
	if action := r.PathValue("action"); action != "" && action != "delete" {
		http.NotFound(w, r)
		return
	}
	form, ok := s.domainForm(w, r)
	if !ok {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("groupID")
	var err error
	if r.PathValue("action") == "delete" {
		err = s.store.DeleteGroup(r.Context(), kind, id, time.Now())
	} else {
		_, err = s.store.SaveGroup(r.Context(), kind, id, storage.GroupInput{Name: form.Get("name"), NodeIDs: form["node_ids"], NodeGroupIDs: form["node_group_ids"]}, time.Now())
	}
	if err != nil {
		writeDomainError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}

func (s *Server) handleGroupsAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("groupID")
	if r.Method == http.MethodGet {
		groups, err := s.store.ListGroups(r.Context(), kind)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "csrf": session.csrf})
		return
	}
	if !s.csrfValid(r, session.csrf, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "请求校验失败", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteGroup(r.Context(), kind, id, time.Now()); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	var input storage.GroupInput
	if err := decodeLimitedJSON(w, r, &input, 64<<10); err != nil {
		http.Error(w, "组消息格式无效", http.StatusBadRequest)
		return
	}
	group, err := s.store.SaveGroup(r.Context(), kind, id, input, time.Now())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, group)
}

func writeDomainError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "操作暂时无法完成。"
	switch {
	case errors.Is(err, storage.ErrGroupNotFound), errors.Is(err, storage.ErrUserNotFound):
		status, message = http.StatusNotFound, "用户、组或关联对象不存在。"
	case errors.Is(err, storage.ErrInvalidUser), errors.Is(err, storage.ErrInvalidGroup):
		status, message = http.StatusBadRequest, "字段、组关系、到期时间或额度无效。"
	case errors.Is(err, storage.ErrNameConflict):
		status, message = http.StatusConflict, "名称已被使用。"
	}
	http.Error(w, message, status)
}
