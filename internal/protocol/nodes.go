package protocol

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"

	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/security"
)

// NormalizeHost accepts literal IP addresses or DNS names, never URLs or ports.
func NormalizeHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(host, "."))
	if err != nil || len(ascii) == 0 || len(ascii) > 253 {
		return "", errors.New("主机名无效")
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", errors.New("主机名无效")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("主机名无效")
			}
		}
	}
	return strings.ToLower(ascii), nil
}

func ValidText(text string, maximum int) bool {
	if strings.TrimSpace(text) == "" || len([]rune(text)) > maximum {
		return false
	}
	for _, c := range text {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

func ValidateDesiredState(state DesiredState) error {
	if len(state.PeriodPolicies) > 4096 {
		return errors.New("周期策略数量超限")
	}
	for i, p := range state.PeriodPolicies {
		if err := period.CheckPublication(state.PeriodPolicies[:i], p); err != nil {
			return err
		}
	}
	if state.Nodes == nil {
		return errors.New("完整快照必须包含 nodes 列表")
	}
	ids, ports, emails := map[string]bool{}, map[int]bool{}, map[string]bool{}
	for _, node := range state.Nodes {
		if !security.IsUUIDv4(node.ID) || ids[node.ID] || !ValidText(node.Name, 128) {
			return errors.New("节点身份或名称无效")
		}
		ids[node.ID] = true
		if _, err := NormalizeHost(node.PublicHost); err != nil || node.PublicPort < 1 || node.PublicPort > 65535 {
			return errors.New("公网主机或端口无效")
		}
		if net.ParseIP(node.ListenHost) == nil || node.ListenPort < 1 || node.ListenPort > 65535 || node.ListenPort == CoreStatsPort || ports[node.ListenPort] {
			return errors.New("监听地址、端口或端口唯一性无效")
		}
		ports[node.ListenPort] = true
		if _, err := NormalizeHost(node.Reality.ServerName); err != nil || net.ParseIP(node.Reality.ServerName) != nil {
			return errors.New("Reality server_name 必须是域名")
		}
		targetHost, targetPort, err := net.SplitHostPort(node.Reality.Target)
		if err != nil {
			return errors.New("Reality target 必须包含主机和端口")
		}
		if _, err := NormalizeHost(targetHost); err != nil {
			return err
		}
		port, err := strconv.Atoi(targetPort)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("Reality target 端口无效")
		}
		key, err := base64.RawURLEncoding.DecodeString(node.Reality.PrivateKey)
		if err != nil {
			return errors.New("Reality private_key 编码无效")
		}
		private, err := ecdh.X25519().NewPrivateKey(key)
		if err != nil || base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()) != node.Reality.PublicKey {
			return errors.New("Reality 密钥不匹配")
		}
		shortID, err := hex.DecodeString(node.Reality.ShortID)
		if err != nil || len(shortID) != 8 {
			return errors.New("Reality short_id 无效")
		}
		users, uuids := map[string]bool{}, map[string]bool{}
		for _, credential := range node.Credentials {
			if credential.ExpiresAt != "" {
				if _, err := time.Parse(time.RFC3339, credential.ExpiresAt); err != nil {
					return errors.New("凭证到期时间无效")
				}
			}
			if !security.IsUUIDv4(credential.UUID) || uuids[credential.UUID] || !ValidText(credential.UserID, 128) || users[credential.UserID] || !ValidText(credential.Email, 128) || strings.Contains(credential.Email, ">") || emails[credential.Email] {
				return fmt.Errorf("节点 %s 的凭证或统计身份无效", node.ID)
			}
			users[credential.UserID], emails[credential.Email] = true, true
			uuids[credential.UUID] = true
		}
	}
	return nil
}
