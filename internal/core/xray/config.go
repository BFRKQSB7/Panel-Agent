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
	inbounds := []any{}
	developmentTags := []string{}
	result := RenderedConfig{Mappings: []StatMapping{}, Listeners: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(protocol.CoreStatsPort))}}
	result.Bindings = append([]string{}, result.Listeners...)
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		credentials := append([]protocol.ProxyCredential{}, node.Credentials...)
		sort.Slice(credentials, func(i, j int) bool { return credentials[i].UserID < credentials[j].UserID })
		clients := []any{}
		onlyDevelopment := len(credentials) > 0
		for _, credential := range credentials {
			if credential.ExpiresAt != "" {
				expiry, _ := time.Parse(time.RFC3339, credential.ExpiresAt)
				if !time.Now().Before(expiry) {
					continue
				}
			}
			onlyDevelopment = onlyDevelopment && credential.Development
			clients = append(clients, map[string]any{"id": credential.UUID, "email": credential.Email, "level": 0})
			result.Mappings = append(result.Mappings, StatMapping{UserID: credential.UserID, NodeID: node.ID, Email: credential.Email})
		}
		if developmentLoopback && onlyDevelopment {
			developmentTags = append(developmentTags, "node-"+node.ID)
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
	}
	config := map[string]any{
		"log":   map[string]any{"access": "none", "error": "none", "loglevel": "none"},
		"api":   map[string]any{"tag": "api", "listen": net.JoinHostPort("127.0.0.1", strconv.Itoa(protocol.CoreStatsPort)), "services": []string{"StatsService"}},
		"stats": map[string]any{}, "policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}},
		"inbounds": inbounds, "outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
	if len(developmentTags) > 0 {
		config["outbounds"] = []any{map[string]any{"protocol": "freedom", "tag": "direct"}, map[string]any{
			"protocol": "freedom", "tag": "development-direct", "settings": map[string]any{"finalRules": []any{
				map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": "18080"},
				map[string]any{"action": "allow", "network": "udp", "ip": []string{"127.0.0.1/32"}, "port": "18081"},
			}},
		}}
		config["routing"] = map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": developmentTags, "outboundTag": "development-direct"}}}
	}
	var err error
	result.Bytes, err = json.MarshalIndent(config, "", "  ")
	return result, err
}
