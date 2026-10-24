package xray

import (
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
)

type StatMapping struct {
	Kind   string `json:"kind,omitempty"`
	UserID string `json:"user_id"`
	NodeID string `json:"node_id"`
	Email  string `json:"email"`
}

type RenderedConfig struct {
	Bytes     []byte
	Mappings  []StatMapping
	Listeners []string
	Bindings  []string
}

func RenderConfig(state protocol.DesiredState) (RenderedConfig, error) {
	return renderConfig(state, false)
}

// RenderDevelopmentConfig permits only the two isolated benchmark targets, and
// only on Nodes containing exclusively explicit development credentials.
func RenderDevelopmentConfig(state protocol.DesiredState) (RenderedConfig, error) {
	return renderConfig(state, true)
}

func renderConfig(state protocol.DesiredState, developmentLoopback bool) (RenderedConfig, error) {
	if err := protocol.ValidateDesiredState(state); err != nil {
		return RenderedConfig{}, err
	}
	nodes := append([]protocol.DesiredNode{}, state.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	globalRestrictions := map[string]bool{}
	nodeRestrictions := map[string]map[string]bool{}
	for _, item := range state.QuotaRestrictions.Items {
		if item.NodeID == "" {
			globalRestrictions[item.UserID] = true
			continue
		}
		if nodeRestrictions[item.NodeID] == nil {
			nodeRestrictions[item.NodeID] = map[string]bool{}
		}
		nodeRestrictions[item.NodeID][item.UserID] = true
	}
	inbounds := []any{}
	developmentTags := []string{}
	routeRules := []any{}
	result := RenderedConfig{Mappings: []StatMapping{}, Listeners: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(protocol.CoreStatsPort))}}
	result.Bindings = append([]string{}, result.Listeners...)
	for _, node := range nodes {
		if !node.Enabled || state.QuotaRestrictions.MachineStop {
			continue
		}
		credentials := append([]protocol.ProxyCredential{}, node.Credentials...)
		sort.Slice(credentials, func(i, j int) bool { return credentials[i].UserID < credentials[j].UserID })
		activeCredentials := make([]protocol.ProxyCredential, 0, len(credentials))
		for _, credential := range credentials {
			if globalRestrictions[credential.UserID] || nodeRestrictions[node.ID][credential.UserID] {
				continue
			}
			activeCredentials = append(activeCredentials, credential)
		}
		clients := []any{}
		onlyDevelopment := len(activeCredentials) > 0
		for _, credential := range activeCredentials {
			if credential.ExpiresAt != "" {
				expiry, _ := time.Parse(time.RFC3339, credential.ExpiresAt)
				if !time.Now().Before(expiry) {
					continue
				}
			}
			onlyDevelopment = onlyDevelopment && credential.Development
			clients = append(clients, map[string]any{"id": credential.UUID, "email": credential.Email, "level": 0})
			result.Mappings = append(result.Mappings, StatMapping{Kind: "user_entry", UserID: credential.UserID, NodeID: node.ID, Email: credential.Email})
		}
		if developmentLoopback && onlyDevelopment {
			developmentTags = append(developmentTags, "node-"+node.ID)
		}
		if node.Extension != nil {
			clients = []any{}
			for _, credential := range activeCredentials {
				if credential.ExpiresAt != "" {
					expiry, _ := time.Parse(time.RFC3339, credential.ExpiresAt)
					if !time.Now().Before(expiry) {
						continue
					}
				}
				clients = append(clients, map[string]any{"password": credential.UUID, "email": credential.Email, "level": 0})
			}
			inbounds = append(inbounds, map[string]any{
				"tag": "node-" + node.ID, "listen": node.ListenHost, "port": node.ListenPort, "protocol": "trojan",
				"settings": map[string]any{"clients": clients},
				"streamSettings": map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{
					"certificates": []any{map[string]any{"certificate": []string{node.Extension.CertificatePEM}, "key": []string{node.Extension.PrivateKeyPEM}}},
				}},
			})
			listenHost := node.ListenHost
			if ip := net.ParseIP(listenHost); ip.IsUnspecified() {
				if ip.To4() != nil {
					listenHost = "127.0.0.1"
				} else {
					listenHost = "::1"
				}
			}
			result.Listeners = append(result.Listeners, net.JoinHostPort(listenHost, strconv.Itoa(node.ListenPort)))
			result.Bindings = append(result.Bindings, net.JoinHostPort(node.ListenHost, strconv.Itoa(node.ListenPort)))
			continue
		}
		inbounds = append(inbounds, map[string]any{
			"tag": "node-" + node.ID, "listen": node.ListenHost, "port": node.ListenPort, "protocol": "vless",
			"settings": map[string]any{"clients": clients, "decryption": "none"},
			"streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{
				"show": false, "target": node.Reality.Target, "serverNames": []string{node.Reality.ServerName},
				"privateKey": node.Reality.PrivateKey, "shortIds": []string{node.Reality.ShortID},
			}},
		})
		listenHost := node.ListenHost
		if ip := net.ParseIP(listenHost); ip.IsUnspecified() {
			if ip.To4() != nil {
				listenHost = "127.0.0.1"
			} else {
				listenHost = "::1"
			}
		}
		result.Listeners = append(result.Listeners, net.JoinHostPort(listenHost, strconv.Itoa(node.ListenPort)))
		result.Bindings = append(result.Bindings, net.JoinHostPort(node.ListenHost, strconv.Itoa(node.ListenPort)))
		if node.ExitMachineID != "" {
			routeRules = append(routeRules, map[string]any{"type": "field", "inboundTag": []string{"node-" + node.ID}, "outboundTag": "exit-" + node.ExitMachineID})
		}
	}
	if state.Exit != nil && !state.QuotaRestrictions.MachineStop {
		clients := make([]any, 0, len(state.Exit.Credentials))
		credentials := append([]protocol.TransitCredential{}, state.Exit.Credentials...)
		sort.Slice(credentials, func(i, j int) bool { return credentials[i].EntryMachineID < credentials[j].EntryMachineID })
		for _, credential := range credentials {
			clients = append(clients, map[string]any{"id": credential.UUID, "email": credential.Email, "level": 0})
			result.Mappings = append(result.Mappings, StatMapping{Kind: "machine_only_transit", Email: credential.Email})
		}
		result.Listeners = append(result.Listeners, net.JoinHostPort(state.Exit.ListenHost, strconv.Itoa(state.Exit.ListenPort)))
		result.Bindings = append(result.Bindings, net.JoinHostPort(state.Exit.ListenHost, strconv.Itoa(state.Exit.ListenPort)))
		inbounds = append(inbounds, map[string]any{
			"tag": "exit-" + state.Exit.MachineID, "listen": state.Exit.ListenHost, "port": state.Exit.ListenPort, "protocol": "vless",
			"settings": map[string]any{"clients": clients, "decryption": "none"},
			"streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{
				"show": false, "target": state.Exit.Reality.Target, "serverNames": []string{state.Exit.Reality.ServerName},
				"privateKey": state.Exit.Reality.PrivateKey, "shortIds": []string{state.Exit.Reality.ShortID},
			}},
		})
	}
	outbounds := []any{map[string]any{"protocol": "freedom", "tag": "direct"}}
	if !state.QuotaRestrictions.MachineStop {
		for _, outbound := range state.TransitOutbounds {
			outbounds = append(outbounds, map[string]any{
				"protocol": "vless", "tag": "exit-" + outbound.ExitMachineID,
				"settings": map[string]any{"vnext": []any{map[string]any{"address": outbound.PublicHost, "port": outbound.PublicPort, "users": []any{map[string]any{"id": outbound.UUID, "encryption": "none"}}}}},
				"streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{
					"fingerprint": "chrome", "serverName": outbound.ServerName, "publicKey": outbound.PublicKey, "shortId": outbound.ShortID,
				}},
			})
		}
	}
	config := map[string]any{
		"log":   map[string]any{"access": "none", "error": "none", "loglevel": "none"},
		"api":   map[string]any{"tag": "api", "listen": net.JoinHostPort("127.0.0.1", strconv.Itoa(protocol.CoreStatsPort)), "services": []string{"StatsService"}},
		"stats": map[string]any{}, "policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}},
		"inbounds": inbounds, "outbounds": outbounds,
	}
	if len(routeRules) > 0 {
		config["routing"] = map[string]any{"rules": routeRules}
	}
	if len(developmentTags) > 0 {
		configOutbounds := append([]any{}, outbounds...)
		configOutbounds = append(configOutbounds, map[string]any{
			"protocol": "freedom", "tag": "development-direct", "settings": map[string]any{"finalRules": []any{
				map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": "18080"},
				map[string]any{"action": "allow", "network": "udp", "ip": []string{"127.0.0.1/32"}, "port": "18081"},
			}},
		})
		config["outbounds"] = configOutbounds
		if routing, ok := config["routing"].(map[string]any); ok {
			routing["rules"] = append([]any{map[string]any{"type": "field", "inboundTag": developmentTags, "outboundTag": "development-direct"}}, routing["rules"].([]any)...)
		} else {
			config["routing"] = map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": developmentTags, "outboundTag": "development-direct"}}}
		}
	}
	var err error
	result.Bytes, err = json.MarshalIndent(config, "", "  ")
	return result, err
}
