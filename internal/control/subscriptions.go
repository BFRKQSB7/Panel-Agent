package control

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

const singBoxRealityUnsupported = "sing-box 1.14.3 无法与固定 Xray 26.10.21 的 Reality 互通，已禁用此组合的导出。请使用 URI（Xray 客户端）或 Mihomo 1.19.33；后者需要 support-x25519mlkem768。"

// This is the complete public projection. Do not serialize NodeRecord or a
// MachineRecord: they also carry topology and management-only identity fields.
type subscriptionNode struct {
	ID, Name, Host, ServerName, PublicKey, ShortID string
	ExtensionKind, ExtensionSNI                    string
	Port                                           int
}

func subscriptionNodes(access storage.UserAccess) []subscriptionNode {
	nodes := make([]subscriptionNode, 0, len(access.Nodes))
	for _, n := range access.Nodes {
		nodes = append(nodes, subscriptionNode{ID: n.NodeID, Name: n.Name, Host: n.PublicHost, Port: n.PublicPort, ServerName: n.ServerName, PublicKey: n.PublicKey, ShortID: n.ShortID, ExtensionKind: n.ExtensionKind, ExtensionSNI: n.ExtensionSNI})
	}
	return nodes
}

func nodeURIAddress(n subscriptionNode) string { return net.JoinHostPort(n.Host, strconv.Itoa(n.Port)) }

func nodeURI(n subscriptionNode, uuid string) string {
	if n.ExtensionKind == "trojan_tls" {
		u := url.URL{Scheme: "trojan", User: url.User(uuid), Host: nodeURIAddress(n), RawQuery: url.Values{"security": {"tls"}, "sni": {n.ExtensionSNI}, "allowInsecure": {"0"}, "type": {"tcp"}}.Encode(), Fragment: n.Name}
		return u.String()
	}
	if n.ExtensionKind != "" {
		return ""
	}
	query := url.Values{"encryption": {"none"}, "security": {"reality"}, "sni": {n.ServerName}, "fp": {"chrome"}, "pbk": {n.PublicKey}, "sid": {n.ShortID}, "type": {"tcp"}}
	u := url.URL{Scheme: "vless", User: url.User(uuid), Host: nodeURIAddress(n), RawQuery: query.Encode(), Fragment: n.Name}
	return u.String()
}

// Rendering a proxy is separate from the import preset (listeners, selection
// group and routing). JSON is a YAML subset accepted by the native Mihomo CLI.
func mihomoProxy(n subscriptionNode, uuid string) map[string]any {
	if n.ExtensionKind == "trojan_tls" {
		return map[string]any{"name": n.Name + " · " + n.ID, "type": "trojan", "server": n.Host, "port": n.Port, "password": uuid, "sni": n.ExtensionSNI, "tls": true, "udp": false}
	}
	if n.ExtensionKind != "" {
		return nil
	}
	return map[string]any{
		"name": n.Name + " · " + n.ID, "type": "vless", "server": n.Host, "port": n.Port,
		"uuid": uuid, "udp": true, "network": "tcp", "tls": true, "servername": n.ServerName,
		"client-fingerprint": "chrome", "reality-opts": map[string]any{"public-key": n.PublicKey, "short-id": n.ShortID, "support-x25519mlkem768": true},
	}
}

func mihomoPreset(nodes []subscriptionNode, uuid string) map[string]any {
	proxies, names := []map[string]any{}, []string{}
	for _, n := range nodes {
		proxy := mihomoProxy(n, uuid)
		if proxy == nil {
			continue
		}
		proxies = append(proxies, proxy)
		names = append(names, proxy["name"].(string))
	}
	if len(names) == 0 {
		names = []string{"REJECT"}
	}
	rules := []string{"DOMAIN,localhost,DIRECT", "DOMAIN-SUFFIX,local,DIRECT"}
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8"} {
		rules = append(rules, "IP-CIDR,"+cidr+",DIRECT,no-resolve")
	}
	rules = append(rules, "RULE-SET,cn-domain,DIRECT", "RULE-SET,cn-ip,DIRECT", "MATCH,PROXY")
	provider := func(behavior, path string) map[string]any {
		return map[string]any{"type": "http", "behavior": behavior, "format": "mrs", "url": "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/" + path + "/cn.mrs", "path": "./rules/" + path + "-cn.mrs", "interval": 86400, "proxy": "DIRECT"}
	}
	return map[string]any{
		"mixed-port": 7890, "bind-address": "127.0.0.1", "allow-lan": false, "mode": "rule", "log-level": "warning", "ipv6": true,
		"proxies": proxies, "proxy-groups": []map[string]any{{"name": "PROXY", "type": "select", "proxies": names}},
		"rule-providers": map[string]any{"cn-domain": provider("domain", "geosite"), "cn-ip": provider("ipcidr", "geoip")}, "rules": rules,
	}
}

func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.UserForCapability(r.Context(), "subscription", r.PathValue("token"))
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	access, err := s.store.GetUserAccess(r.Context(), id, time.Now())
	if err != nil {
		s.capabilityError(w, err)
		return
	}
	nodes := subscriptionNodes(access)
	switch r.PathValue("format") {
	case "uri":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="nodes.txt"`)
		for _, n := range nodes {
			_, _ = w.Write([]byte(nodeURI(n, access.User.CredentialUUID) + "\n"))
		}
	case "mihomo":
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="mihomo.yaml"`)
		_ = json.NewEncoder(w).Encode(mihomoPreset(nodes, access.User.CredentialUUID))
	case "sing-box":
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_core_client_combination", "message": singBoxRealityUnsupported})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) capabilityError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrCapabilityNotFound) || errors.Is(err, storage.ErrUserNotFound) {
		http.Error(w, "链接无效或已撤销", http.StatusNotFound)
		return
	}
	http.Error(w, "页面暂时无法读取", http.StatusInternalServerError)
}
