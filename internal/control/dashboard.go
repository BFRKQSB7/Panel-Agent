package control

import (
	"net/http"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/quantity"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

type dashboardNode struct {
	Name, Address, Status, URI           string
	Usage, Limit, Remaining, QuotaStatus string
	Pending                              bool
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r, nil)
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, results []storage.TokenDelivery) {
	id, err := s.store.UserForCapability(r.Context(), "dashboard", r.PathValue("token"))
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	access, err := s.store.GetUserAccess(r.Context(), id, time.Now())
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	quota, err := s.store.GetUserQuotaSummary(r.Context(), id, time.Now())
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	state, err := s.store.GetUserTokenState(r.Context(), id)
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	subject, err := s.tokenActionSubject(r.Context(), id+":"+security.HashToken(r.PathValue("token")))
	if err != nil {
		writeTokenError(w, err)
		return
	}
	action, err := s.actions.Issue("user-reset-subscription", subject, time.Now())
	if err != nil {
		writeTokenError(w, err)
		return
	}
	rows := []dashboardNode{}
	quotaByNode := map[string]storage.UserNodeQuotaView{}
	for _, item := range quota.Nodes {
		quotaByNode[item.NodeID] = item
	}
	public := subscriptionNodes(access)
	for i, node := range access.Nodes {
		machine, err := s.store.GetMachine(r.Context(), node.MachineID)
		if err != nil {
			writeTokenError(w, err)
			return
		}
		status := "心跳失联或尚未上报（保留节点定义）"
		if machine.Revoked {
			status = "机器接入凭据已吊销"
		} else if heartbeat, err := time.Parse(time.RFC3339Nano, machine.LastHeartbeatAt); err == nil && time.Since(heartbeat) < 2*time.Minute {
			status = "近期有心跳；当前配置待应用"
			if machine.AppliedRevision == machine.DesiredRevision && machine.AppliedDigest == machine.DesiredDigest && machine.AppliedAuthorityEpoch == machine.AuthorityEpoch && machine.ApplyError == "" {
				status = "近期有心跳；Agent 已应用配置（未做外网连通性检查）"
			}
		}
		nodeRow := dashboardNode{Name: node.Name, Address: nodeURIAddress(public[i]), Status: status, URI: nodeURI(public[i], access.User.CredentialUUID), Limit: "不限", Remaining: "—", QuotaStatus: "无限制"}
		if item, found := quotaByNode[node.NodeID]; found {
			nodeRow.Usage = item.Usage.UsedBytes.String()
			if item.EffectiveQuota != nil {
				nodeRow.Limit = item.EffectiveQuota.String()
				if item.Usage.RemainingBytes != nil {
					nodeRow.Remaining = item.Usage.RemainingBytes.String()
				}
				nodeRow.QuotaStatus = quotaUsageStatus(item.Usage.Status, item.Usage.PendingReconcile)
			}
			nodeRow.Pending = item.Usage.PendingReconcile
		}
		rows = append(rows, nodeRow)
	}
	reasons := []string{}
	for _, reason := range access.UnavailableReasons {
		switch reason {
		case "expired":
			reasons = append(reasons, "用户已到期")
		case "manually_disabled":
			reasons = append(reasons, "用户已停用")
		case "group_deleted":
			reasons = append(reasons, "用户组已删除")
		}
	}
	for _, reason := range quota.Reasons {
		switch reason {
		case "user_quota_exhausted":
			reasons = append(reasons, "全局用户额度已耗尽")
		case "node_quota_exhausted":
			reasons = append(reasons, "节点额度已耗尽")
		}
	}
	tokenPageHeaders(w)
	globalLimit, globalRemaining := "不限", "—"
	if quota.Global.LimitBytes != nil {
		globalLimit = quota.Global.LimitBytes.String()
		if quota.Global.RemainingBytes != nil {
			globalRemaining = quota.Global.RemainingBytes.String()
		}
	}
	_ = s.templates.ExecuteTemplate(w, "dashboard.html", map[string]any{"UserID": id, "Name": access.User.Name, "ExpiresAt": access.User.ExpiresAt, "QuotaBytes": access.User.QuotaBytes, "Quota": quota, "GlobalUsed": quota.Global.UsedBytes.String(), "GlobalLimit": globalLimit, "GlobalRemaining": globalRemaining, "GlobalStatus": quotaUsageStatus(quota.Global.Status, quota.Global.PendingReconcile), "GlobalUpdated": quota.Global.LastUpdated, "Nodes": rows, "Reasons": reasons, "State": state, "ActionToken": action, "Results": results, "ResetPath": "/u/" + r.PathValue("token") + "/subscription/reset", "SingBoxNotice": singBoxRealityUnsupported})
}

func quotaUsageStatus(status []string, pending bool) string {
	if pending {
		return "待对账（限制保持当前已确认状态）"
	}
	if len(status) > 0 && status[0] == "exhausted" {
		return "已耗尽"
	}
	if len(status) > 0 && status[0] == "unknown_policy" {
		return "周期策略待生成"
	}
	return "正常"
}

func quotaText(value *quantity.Bytes) string {
	if value == nil {
		return "不限"
	}
	return value.String()
}

func (s *Server) handleSubscriptionReset(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.UserForCapability(r.Context(), "dashboard", r.PathValue("token"))
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	if r.Header.Get("Origin") == "" || !s.sameOrigin(r) {
		http.Error(w, "请求来源无效", http.StatusForbidden)
		return
	}
	var input struct {
		ActionToken string                  `json:"action_token"`
		Spec        security.CapabilitySpec `json:"spec"`
	}
	jsonRequest := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if jsonRequest {
		if err := decodeLimitedJSON(w, r, &input, 8<<10); err != nil {
			http.Error(w, "请求格式无效", http.StatusBadRequest)
			return
		}
	} else {
		form, err := readForm(w, r)
		if err != nil {
			http.Error(w, "请求格式无效", http.StatusBadRequest)
			return
		}
		for key, values := range form {
			if (key != "action_token" && key != "prefix" && key != "value") || len(values) != 1 {
				http.Error(w, "用户仅能重置自己的订阅 Token", http.StatusBadRequest)
				return
			}
		}
		input.ActionToken = form.Get("action_token")
		input.Spec = security.CapabilitySpec{Prefix: form.Get("prefix"), Value: form.Get("value")}
	}
	subject, err := s.tokenActionSubject(r.Context(), id+":"+security.HashToken(r.PathValue("token")))
	if err != nil {
		writeTokenError(w, err)
		return
	}
	op, err := s.actions.Verify(input.ActionToken, "user-reset-subscription", subject, time.Now())
	if err != nil {
		writeTokenError(w, errInvalidTokenAction)
		return
	}
	delivery, err := s.store.ResetUserToken(r.Context(), op, "subscription", storage.TokenMutation{UserID: id, Spec: input.Spec}, time.Now())
	if err != nil {
		writeTokenError(w, err)
		return
	}
	if jsonRequest {
		writeJSON(w, http.StatusOK, delivery)
		return
	}
	s.renderDashboard(w, r, []storage.TokenDelivery{delivery})
}
