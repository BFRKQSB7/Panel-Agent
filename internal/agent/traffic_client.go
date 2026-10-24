package agent

import (
	"context"
	"errors"
	"log"
	"time"

	"panel-agent.local/panel-agent/internal/protocol"
)

// Called under syncMu after the control authority handshake. D08's receiver is
// a deterministic fixture; production snapshots enable this only with D09 ingest.
func (c *Client) syncTraffic(ctx context.Context) error {
	if c.ledger == nil {
		return nil
	}
	request, err := c.ledger.RecoveryRequest(ctx, c.identity.AuthorityEpoch)
	if err != nil {
		return err
	}
	var response protocol.TrafficRecoveryResponse
	if _, err = c.postJSON(ctx, "/api/v1/agent/traffic/recover", request, c.identity.MachineSecret, &response); err != nil {
		return err
	}
	if err = c.ledger.ConfirmRecovery(ctx, request, response); err != nil {
		return err
	}
	report, err := c.ledger.Pending(ctx, c.identity.AuthorityEpoch, time.Now().UTC())
	if err != nil || len(report.Segments) == 0 {
		return err
	}
	var ack protocol.TrafficACK
	if _, err = c.postJSON(ctx, "/api/v1/agent/traffic", report, c.identity.MachineSecret, &ack); err != nil {
		return err
	}
	return c.ledger.ConsumeACK(ctx, report, ack, time.Now().UTC())
}

func (c *Client) runSampler(ctx context.Context) {
	failed := false
	lowDisk := false
	nextDiskCheck := time.Time{}
	for ctx.Err() == nil {
		if time.Now().After(nextDiskCheck) {
			if d, err := c.ledger.Diagnostic(ctx); err == nil {
				if d.LowDisk && !lowDisk {
					log.Print("Agent 账本可用磁盘低于 16MiB；未确认流量不会删除，请释放磁盘空间。")
				}
				lowDisk = d.LowDisk
			}
			nextDiskCheck = time.Now().Add(time.Minute)
		}
		err := c.core.Sample(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			if !failed {
				log.Print("流量采样未提交，保留原水位并重试；请检查账本诊断与可用磁盘。")
			}
			failed = true
		} else if failed {
			log.Print("流量采样已恢复")
			failed = false
		}
		wait := c.core.nextSample()
		if failed {
			wait = max(wait, 5*time.Second)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
