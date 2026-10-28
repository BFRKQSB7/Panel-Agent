//go:build !linux

package agent

import (
	"context"
	"errors"
	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/protocol"
)

type hysteriaManager struct{}

func (*hysteriaManager) Status() CoreApplyStatus { return CoreApplyStatus{} }

func newHysteriaManager(context.Context, string, string, *ledger.Store) (*hysteriaManager, error) {
	return nil, errors.New("官方 Hysteria 受管实例需要 Linux")
}
func (*hysteriaManager) Apply(context.Context, string, uint64, string, protocol.DesiredState) error {
	return errors.New("官方 Hysteria 受管实例需要 Linux")
}
func (*hysteriaManager) Sample(context.Context) error        { return nil }
func (*hysteriaManager) Reconcile(context.Context) error     { return nil }
func (*hysteriaManager) Matches(string, uint64, string) bool { return false }
func (*hysteriaManager) Close()                              {}

func (c *Client) UpgradeHysteria(context.Context, string, string, string) error {
	return errors.New("Hysteria upgrade requires Linux")
}
