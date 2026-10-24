package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/storage"
)

const (
	connectivityModePublic = "public"
	connectivityModeLocal  = "local"
)

var (
	errProbeMode   = errors.New("不支持的探测模式")
	errProbeTarget = errors.New("探测目标不允许")
)

type guideStep struct {
	Label string
	Text  string
}

type nodeGuide struct {
	NAT         []guideStep
	DNS         []guideStep
	Protocol    []guideStep
	AgentStatus string
	CoreStatus  string
	ProbeStatus string
	ProbePath   string
	LocalProbe  bool
	PublicHost  string
	PublicPort  int
	ListenHost  string
	ListenPort  int
}

func makeNodeGuide(machineID string, node storage.NodeRecord, machine protocol.MachineView) nodeGuide {
	publicEndpoint := net.JoinHostPort(node.PublicHost, strconv.Itoa(node.PublicPort))
	listenEndpoint := net.JoinHostPort(node.ListenHost, strconv.Itoa(node.ListenPort))
	guide := nodeGuide{
		NAT: []guideStep{{Label: "TCP 端口映射", Text: fmt.Sprintf("公网 TCP %s -> 本机 %s", publicEndpoint, listenEndpoint)}},
		DNS: []guideStep{},
		Protocol: []guideStep{
			{Label: "协议", Text: fmt.Sprintf("VLESS + REALITY + RAW；SNI=%s；目标=%s；公网入口=%s", node.ServerName, node.Target, publicEndpoint)},
			{Label: "客户端参数", Text: fmt.Sprintf("公网地址=%s；SNI=%s；公钥=%s；Short ID=%s；传输=tcp", publicEndpoint, node.ServerName, node.PublicKey, node.ShortID)},
			{Label: "证书与 UDP", Text: "Reality 不需要上传普通 TLS 证书；不要把私钥放入 DNS、订阅或客户端。若客户端业务需要 UDP，需单独确认运营商支持 UDP 映射和该协议能力；本检查只验证 TCP，不把 TCP 成功当作 UDP 或代理协议成功。"},
		},
		AgentStatus: "未收到 Agent 心跳；不能据此判断公网连通性",
		CoreStatus:  "尚无已应用快照",
		ProbeStatus: "检查端点由 Panel 控制进程发起，只验证一次 DNS 解析和 TCP 建连，不验证 Reality 握手、转发或业务出口",
		ProbePath:   "/api/admin/machines/" + url.PathEscape(machineID) + "/nodes/" + url.PathEscape(node.NodeID) + "/connectivity-check",
		LocalProbe:  isLoopbackLiteral(node.PublicHost),
		PublicHost:  node.PublicHost,
		PublicPort:  node.PublicPort,
		ListenHost:  node.ListenHost,
		ListenPort:  node.ListenPort,
	}
	if machine.LastHeartbeat != "" {
		guide.AgentStatus = "最近收到 Agent 心跳：" + machine.LastHeartbeat + "；心跳在线不代表公网入口可达"
	}
	if machine.ApplyError != "" {
		guide.CoreStatus = "Core 应用失败：" + machine.ApplyError + "；当前可能仍在运行旧配置"
	} else if machine.AppliedRevision == machine.DesiredRevision && machine.AppliedRevision != 0 {
		guide.CoreStatus = fmt.Sprintf("Core 已应用 revision %d；仍需单独检查监听和公网路径", machine.AppliedRevision)
	} else if machine.DesiredRevision != 0 {
		guide.CoreStatus = fmt.Sprintf("Core 尚未确认应用目标 revision %d；已应用 revision %d", machine.DesiredRevision, machine.AppliedRevision)
	}

	if ip := net.ParseIP(node.PublicHost); ip != nil {
		recordType := "AAAA"
		if ip.To4() != nil {
			recordType = "A"
		}
		guide.DNS = append(guide.DNS, guideStep{Label: "DNS", Text: fmt.Sprintf("当前公网主机是字面量 IP，无需 DNS；若改用域名，为该域名创建 %s 记录并指向已确认的公网地址，不要填写 %s", recordType, node.ListenHost)})
	} else {
		guide.DNS = append(guide.DNS,
			guideStep{Label: "A 记录", Text: fmt.Sprintf("A %s -> <已确认的公网 IPv4>；A/AAAA 记录不携带端口", node.PublicHost)},
			guideStep{Label: "AAAA 记录", Text: fmt.Sprintf("AAAA %s -> <已确认的公网 IPv6>；没有 IPv6 时不要伪造 AAAA", node.PublicHost)},
			guideStep{Label: "DNS 代理", Text: "若使用 Cloudflare，Reality 节点默认选择 DNS only；不要为兼容性教程默认打开 CDN/代理，先确认实际协议支持。"},
		)
	}
	return guide
}

type ipResolver interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
}

type probeDialer func(context.Context, string, string) (net.Conn, error)

type connectivityStage struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type connectivityResult struct {
	NodeID     string            `json:"node_id"`
	Mode       string            `json:"mode"`
	Probe      string            `json:"probe"`
	Target     string            `json:"target"`
	ResolvedIP string            `json:"resolved_ip,omitempty"`
	DNS        connectivityStage `json:"dns"`
	TCP        connectivityStage `json:"tcp"`
	OK         bool              `json:"ok"`
	CheckedAt  time.Time         `json:"checked_at"`
}

func normalizeProbeIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], v4[3])
	}
	return ip.To16()
}

func isSharedProbeIPv4(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

func isPublicProbeIP(ip net.IP) bool {
	ip = normalizeProbeIP(ip)
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified() && !isSharedProbeIPv4(ip)
}

func isLoopbackLiteral(host string) bool {
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func selectProbeIP(ctx context.Context, host, mode string, resolver ipResolver) (net.IP, string, error) {
	if mode != connectivityModePublic && mode != connectivityModeLocal {
		return nil, "", errProbeMode
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, "", errProbeTarget
	}
	if ip := normalizeProbeIP(net.ParseIP(host)); ip != nil {
		if mode == connectivityModeLocal {
			if !ip.IsLoopback() {
				return nil, "", fmt.Errorf("%w：local 只允许字面量 loopback", errProbeTarget)
			}
			return ip, "字面量 loopback，未执行 DNS", nil
		}
		if !isPublicProbeIP(ip) {
			return nil, "", fmt.Errorf("%w：公网探测拒绝私网、回环、链路本地或共享地址", errProbeTarget)
		}
		return ip, "字面量公网 IP，未执行 DNS", nil
	}
	if mode == connectivityModeLocal {
		return nil, "", fmt.Errorf("%w：local 不接受域名，避免 DNS 重绑定", errProbeTarget)
	}
	normalized, err := protocol.NormalizeHost(host)
	if err != nil || net.ParseIP(normalized) != nil {
		return nil, "", fmt.Errorf("%w：主机名无效", errProbeTarget)
	}
	ips, err := resolver.LookupIP(ctx, "ip", normalized)
	if err != nil {
		return nil, "", fmt.Errorf("DNS 解析失败：%w", err)
	}
	if len(ips) == 0 {
		return nil, "", errors.New("DNS 解析失败：没有返回地址")
	}
	seen := map[string]bool{}
	var selected net.IP
	for _, raw := range ips {
		ip := normalizeProbeIP(raw)
		if ip == nil || !isPublicProbeIP(ip) {
			return nil, "", fmt.Errorf("%w：DNS 返回了非公网地址", errProbeTarget)
		}
		key := ip.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		if selected == nil {
			selected = ip
		}
	}
	if selected == nil {
		return nil, "", fmt.Errorf("%w：没有可用公网地址", errProbeTarget)
	}
	return selected, fmt.Sprintf("DNS 仅解析一次，已绑定 %s", selected.String()), nil
}

func runConnectivityProbeWith(ctx context.Context, node storage.NodeRecord, mode string, resolver ipResolver, dial probeDialer, now time.Time) connectivityResult {
	result := connectivityResult{NodeID: node.NodeID, Mode: mode, Probe: "Panel 控制进程", Target: net.JoinHostPort(node.PublicHost, strconv.Itoa(node.PublicPort)), CheckedAt: now.UTC()}
	if mode != connectivityModePublic && mode != connectivityModeLocal {
		result.DNS = connectivityStage{Status: "rejected", Detail: errProbeMode.Error()}
		result.TCP = connectivityStage{Status: "not_checked", Detail: "探测模式无效"}
		return result
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	ip, detail, err := selectProbeIP(resolveCtx, node.PublicHost, mode, resolver)
	cancel()
	if err != nil {
		status := "failed"
		if errors.Is(err, errProbeTarget) || errors.Is(err, errProbeMode) {
			status = "rejected"
		}
		result.DNS = connectivityStage{Status: status, Detail: err.Error()}
		result.TCP = connectivityStage{Status: "not_checked", Detail: "DNS/目标校验未通过，未发起 TCP 建连"}
		return result
	}
	result.ResolvedIP = ip.String()
	result.DNS = connectivityStage{Status: "passed", Detail: detail}
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	conn, err := dial(dialCtx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(node.PublicPort)))
	cancel()
	if err != nil {
		result.TCP = connectivityStage{Status: "failed", Detail: "已绑定解析地址，但 TCP 建连失败：" + err.Error()}
		return result
	}
	if conn == nil {
		result.TCP = connectivityStage{Status: "failed", Detail: "探测拨号器没有返回连接"}
		return result
	}
	_ = conn.Close()
	result.TCP = connectivityStage{Status: "passed", Detail: "已绑定解析地址完成 TCP 建连；未验证 Reality 握手或代理业务"}
	result.OK = true
	return result
}

func runConnectivityProbe(ctx context.Context, node storage.NodeRecord, mode string) connectivityResult {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return runConnectivityProbeWith(ctx, node, mode, net.DefaultResolver, dialer.DialContext, time.Now().UTC())
}
