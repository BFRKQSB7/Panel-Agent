package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/core/xray"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

// TestRealCoreOneHopStage is a deliberately opt-in, two-host smoke worker.
// The runner owns its private directory and stops both Cores in finally.
// Each invocation uses the production apply, sampling and durable ledger paths.
func TestRealCoreOneHopStage(t *testing.T) {
	root, action := os.Getenv("PANEL_AGENT_D11_ROOT"), os.Getenv("PANEL_AGENT_D11_ACTION")
	if root == "" || action == "" {
		t.Skip("requires the explicit two-host D11 runner")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("smoke directory must be absolute")
	}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		check(err)
		check(storage.WritePrivateAtomic(filepath.Join(root, name), data))
	}
	type plan struct {
		Authority   string
		EntryID     string
		ExitID      string
		RotatedUUID string
		Entry       protocol.DesiredState
		Exit        protocol.DesiredState
	}
	var p plan
	if action == "init" {
		newID := func() string { id, err := security.NewUUID(); check(err); return id }
		reality := func() protocol.RealityConfig {
			key, err := ecdh.X25519().GenerateKey(rand.Reader)
			check(err)
			return protocol.RealityConfig{ServerName: "www.cloudflare.com", Target: "www.cloudflare.com:443", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0123456789abcdef"}
		}
		port, err := strconv.Atoi(os.Getenv("PANEL_AGENT_D11_EXIT_PORT"))
		check(err)
		host := os.Getenv("PANEL_AGENT_D11_EXIT_HOST")
		if net.ParseIP(host) == nil {
			t.Fatal("explicit literal Exit address required")
		}
		p = plan{Authority: newID(), EntryID: newID(), ExitID: newID(), RotatedUUID: newID(), Entry: protocol.EmptyDesiredState(), Exit: protocol.EmptyDesiredState()}
		user, node, credential, transit := newID(), newID(), newID(), newID()
		exitReality := reality()
		p.Exit.Exit = &protocol.DesiredExit{MachineID: p.ExitID, PublicHost: host, PublicPort: port, ListenHost: "0.0.0.0", ListenPort: port, Reality: exitReality, Credentials: []protocol.TransitCredential{{EntryMachineID: p.EntryID, UUID: transit, Email: "transit-" + p.EntryID}}}
		p.Entry.Nodes = []protocol.DesiredNode{{ID: node, Name: "D11 isolated entry", PublicHost: "127.0.0.1", PublicPort: 19443, ListenHost: "127.0.0.1", ListenPort: 19443, Enabled: true, Reality: reality(), ExitMachineID: p.ExitID, Credentials: []protocol.ProxyCredential{{UserID: user, UUID: credential, Email: "u-" + user + "-n-" + node}}}}
		p.Entry.TransitOutbounds = []protocol.TransitOutbound{{ExitMachineID: p.ExitID, PublicHost: host, PublicPort: port, UUID: transit, ServerName: exitReality.ServerName, PublicKey: exitReality.PublicKey, ShortID: exitReality.ShortID}}
		for _, scope := range []period.Scope{{Kind: "user", UserID: user}, {Kind: "user_node", UserID: user, NodeID: node}, {Kind: "machine", MachineID: p.EntryID}, {Kind: "machine", MachineID: p.ExitID}} {
			policy, err := period.New(scope, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
			check(err)
			if scope.MachineID == p.ExitID {
				p.Exit.PeriodPolicies = append(p.Exit.PeriodPolicies, policy)
			} else {
				p.Entry.PeriodPolicies = append(p.Entry.PeriodPolicies, policy)
			}
		}
		check(protocol.ValidateDesiredState(p.Entry))
		check(protocol.ValidateDesiredState(p.Exit))
		write("plan.json", p)
		r := p.Entry.Nodes[0].Reality
		write("client.json", map[string]any{"log": map[string]any{"loglevel": "warning"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": 10818, "protocol": "socks", "settings": map[string]any{"auth": "noauth", "udp": true}}}, "outbounds": []any{map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": 19443, "users": []any{map[string]any{"id": credential, "encryption": "none"}}}}}, "streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{"serverName": r.ServerName, "fingerprint": "chrome", "publicKey": r.PublicKey, "shortId": r.ShortID}}}}})
		return
	}
	data, err := os.ReadFile(filepath.Join(root, "plan.json"))
	check(err)
	check(json.Unmarshal(data, &p))
	role := os.Getenv("PANEL_AGENT_D11_ROLE")
	desired, machine := p.Entry, p.EntryID
	if role == "exit" {
		desired, machine = p.Exit, p.ExitID
	} else if role != "entry" {
		t.Fatal("invalid role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	dir := filepath.Join(root, "agent")
	book, err := ledger.Open(dir, machine)
	check(err)
	defer book.Close()
	m, err := NewCoreManager(dir, os.Getenv("PANEL_AGENT_TEST_XRAY"), false)
	check(err)
	m.ledger = book
	if action == "stop" {
		processes, err := m.ownedProcesses()
		check(err)
		for _, process := range processes {
			check(stopCoreProcess(ctx, process, m.binary))
		}
		return
	}
	check(m.Recover(ctx))
	switch action {
	case "apply", "rotate", "hard-stop", "disabled", "bad-config":
		if action == "rotate" {
			if role == "entry" {
				desired.TransitOutbounds[0].UUID = p.RotatedUUID
			} else {
				desired.Exit.Credentials[0].UUID = p.RotatedUUID
			}
		}
		if action == "hard-stop" {
			desired.QuotaRestrictions.MachineStop = true
		}
		if action == "disabled" {
			desired.Nodes[0].Enabled = false
			desired.TransitOutbounds = nil
		}
		if action == "bad-config" {
			desired.Nodes[0].Reality.ShortID = "invalid"
		}
		digest, err := protocol.DigestDesiredState(desired)
		check(err)
		err = m.Apply(ctx, p.Authority, m.Status().Revision+1, digest, desired)
		if action == "bad-config" {
			if err == nil || !m.Status().Running {
				t.Fatal("invalid candidate did not retain healthy Core")
			}
		} else {
			check(err)
		}
	case "sample":
		check(m.Sample(ctx))
		stats, err := xray.DialStats(ctx, "127.0.0.1:10085")
		check(err)
		defer stats.Close()
		raw := map[string]int64{}
		for _, mapping := range m.state.Current.Mappings {
			for _, direction := range []string{"uplink", "downlink"} {
				key := "user>>>" + mapping.Email + ">>>traffic>>>" + direction
				raw[key], err = stats.QueryCounter(ctx, key)
				check(err)
			}
		}
		check(book.Close())
		diagnostic, err := ledger.Inspect(ctx, dir)
		check(err)
		if len(diagnostic.PendingPreview) == 0 {
			t.Fatal("real traffic produced no durable segments")
		}
		var up, down uint64
		for _, s := range diagnostic.PendingPreview {
			if role == "exit" && (s.Stream.Kind != "machine_only_transit" || s.Stream.UserID != "" || s.Stream.EntryNodeID != "") {
				t.Fatal("Exit billed user traffic")
			}
			if role == "entry" && s.Stream.Kind != "user_entry" {
				t.Fatal("entry mapping changed")
			}
			up += uint64(s.Totals.Uplink)
			down += uint64(s.Totals.Downlink)
		}
		if up == 0 || down == 0 {
			t.Fatal("missing real bidirectional traffic")
		}
		write("sample.json", map[string]any{"role": role, "raw": raw, "ledger": diagnostic, "uplink": up, "downlink": down})
	default:
		t.Fatal("unknown action")
	}
	write("status.json", m.Status())
}
