package protocol

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
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

func validateReality(reality RealityConfig) error {
	if _, err := NormalizeHost(reality.ServerName); err != nil || net.ParseIP(reality.ServerName) != nil {
		return errors.New("Reality server_name 必须是域名")
	}
	targetHost, targetPort, err := net.SplitHostPort(reality.Target)
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
	key, err := base64.RawURLEncoding.DecodeString(reality.PrivateKey)
	if err != nil {
		return errors.New("Reality private_key 编码无效")
	}
	private, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil || base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()) != reality.PublicKey {
		return errors.New("Reality 密钥不匹配")
	}
	shortID, err := hex.DecodeString(reality.ShortID)
	if err != nil || len(shortID) != 8 {
		return errors.New("Reality short_id 无效")
	}
	return nil
}

func validateTransitOutbound(outbound TransitOutbound) error {
	if !security.IsUUIDv4(outbound.ExitMachineID) || !security.IsUUIDv4(outbound.UUID) {
		return errors.New("Transit 身份无效")
	}
	if _, err := NormalizeHost(outbound.PublicHost); err != nil || outbound.PublicPort < 1 || outbound.PublicPort > 65535 {
		return errors.New("Transit 公网主机或端口无效")
	}
	if _, err := NormalizeHost(outbound.ServerName); err != nil || net.ParseIP(outbound.ServerName) != nil {
		return errors.New("Transit SNI 必须是域名")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(outbound.PublicKey)
	if err != nil || len(publicKey) != 32 {
		return errors.New("Transit 公钥无效")
	}
	shortID, err := hex.DecodeString(outbound.ShortID)
	if err != nil || len(shortID) != 8 {
		return errors.New("Transit short_id 无效")
	}
	return nil
}

func validateExtension(extension ExtensionNode) error {
	if extension.Kind == "hysteria2_tls" && extension.CoreMinVersion != "" && extension.CoreMinVersion != "2.12.3" && extension.CoreMinVersion != "2.13.0" {
		return errors.New("不支持的 Hysteria 最低版本")
	}
	if !ValidText(extension.ID, 64) || !ValidText(extension.Version, 32) || extension.Kind != "trojan_tls" && extension.Kind != "hysteria2_tls" {
		return errors.New("扩展身份或类型无效")
	}
	if _, err := NormalizeHost(extension.SNI); err != nil || net.ParseIP(extension.SNI) != nil {
		return errors.New("扩展 SNI 必须是域名")
	}
	if len(extension.CertificatePEM) == 0 || len(extension.CertificatePEM) > 65536 || len(extension.PrivateKeyPEM) == 0 || len(extension.PrivateKeyPEM) > 65536 {
		return errors.New("扩展证书或私钥大小无效")
	}
	certificate, _ := pem.Decode([]byte(extension.CertificatePEM))
	privateKey, _ := pem.Decode([]byte(extension.PrivateKeyPEM))
	if certificate == nil || certificate.Type != "CERTIFICATE" || privateKey == nil || privateKey.Type != "PRIVATE KEY" && privateKey.Type != "RSA PRIVATE KEY" && privateKey.Type != "EC PRIVATE KEY" {
		return errors.New("扩展证书或私钥 PEM 无效")
	}
	return nil
}

func ValidateDesiredState(state DesiredState) error {
	if err := state.QuotaRestrictions.Validate(); err != nil {
		return err
	}
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
	outbounds := map[string]bool{}
	if state.Exit != nil {
		exit := state.Exit
		if !security.IsUUIDv4(exit.MachineID) || exit.PublicPort < 1 || exit.PublicPort > 65535 || net.ParseIP(exit.ListenHost) == nil || exit.ListenPort < 1 || exit.ListenPort > 65535 || exit.ListenPort == CoreStatsPort {
			return errors.New("Exit 身份、主机或端口无效")
		}
		if _, err := NormalizeHost(exit.PublicHost); err != nil {
			return errors.New("Exit 公网主机无效")
		}
		if ports[exit.ListenPort] {
			return errors.New("Exit 与 Node 监听端口冲突")
		}
		ports[exit.ListenPort] = true
		if err := validateReality(exit.Reality); err != nil {
			return err
		}
		seenEntries, seenUUIDs := map[string]bool{}, map[string]bool{}
		for _, credential := range exit.Credentials {
			if !security.IsUUIDv4(credential.EntryMachineID) || credential.EntryMachineID == exit.MachineID || !security.IsUUIDv4(credential.UUID) || seenEntries[credential.EntryMachineID] || seenUUIDs[credential.UUID] || !ValidText(credential.Email, 128) || strings.Contains(credential.Email, ">") || emails[credential.Email] {
				return errors.New("Transit 凭证无效或重复")
			}
			seenEntries[credential.EntryMachineID], seenUUIDs[credential.UUID], emails[credential.Email] = true, true, true
		}
	}
	for _, outbound := range state.TransitOutbounds {
		if err := validateTransitOutbound(outbound); err != nil || state.Exit != nil && outbound.ExitMachineID == state.Exit.MachineID {
			if err != nil {
				return err
			}
			return errors.New("禁止机器把自身作为 Exit")
		}
		if outbounds[outbound.ExitMachineID] {
			return errors.New("Transit 出口重复")
		}
		outbounds[outbound.ExitMachineID] = true
	}
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
		if err := validateReality(node.Reality); err != nil {
			if node.Extension == nil {
				return err
			}
		}
		if node.Extension != nil {
			if err := validateExtension(*node.Extension); err != nil {
				return err
			}
			if node.ExitMachineID != "" {
				return errors.New("当前扩展不支持 One-Hop Exit")
			}
		}
		if node.ExitMachineID != "" && (!security.IsUUIDv4(node.ExitMachineID) || node.Enabled && !outbounds[node.ExitMachineID]) {
			return errors.New("Node 的 Exit 路由未准备好")
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
	for target := range outbounds {
		used := false
		for _, node := range state.Nodes {
			if node.Enabled && node.ExitMachineID == target {
				used = true
				break
			}
		}
		if !used {
			return errors.New("Transit 出口没有对应的启用 Node")
		}
	}
	return nil
}
