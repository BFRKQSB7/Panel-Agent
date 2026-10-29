package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/protocol"
)

func readModeJournal(path string, target any) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return false, errors.New("unsafe Core mode journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err = json.Unmarshal(data, target); err != nil {
		return false, err
	}
	return true, nil
}

// Omitted Hysteria must have a durably applied removal first. Starting an old
// offline snapshot after a period of unmanaged operation can restore old grants.
func requireHysteriaRemoval(dir string) error {
	var record struct {
		Version int
		Pending bool
		Desired protocol.DesiredState
	}
	exists, err := readModeJournal(filepath.Join(dir, "hysteria-state.json"), &record)
	if err != nil || !exists {
		return err
	}
	if record.Version != 1 {
		return errors.New("invalid Hysteria mode journal")
	}
	if record.Pending {
		return errors.New("请保留 --hysteria-path，先完成待应用配置再移除内核")
	}
	for _, n := range record.Desired.Nodes {
		if n.Extension != nil && n.Extension.Kind == "hysteria2_tls" && n.Enabled {
			return errors.New("请保留 --hysteria-path，先移除或停用 hy2 节点并完成同步")
		}
	}
	return nil
}

// Xray intentionally survives normal Agent exits. A switch to hy2-only must
// stop and finalize the previously owned Xray instead of abandoning it.
func retireOmittedXray(ctx context.Context, dir string, book *ledger.Store) error {
	var record coreJournal
	exists, err := readModeJournal(filepath.Join(dir, "core", "state.json"), &record)
	if err != nil || !exists {
		return err
	}
	if record.Phase != "stable" || record.Boundary != "" {
		return errors.New("请保留 --xray-path，先恢复未完成的 Xray 操作")
	}
	m, err := NewCoreManager(dir, record.Binary, record.DevelopmentLoopback)
	if err != nil {
		return err
	}
	m.ledger = book
	if err = m.stopTransient(ctx); err != nil {
		return err
	}
	next := m.state
	next.Current = nil
	next.LKG = nil
	next.Candidate = nil
	return m.save(next)
}
