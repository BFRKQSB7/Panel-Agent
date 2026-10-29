package agent

import (
	"errors"
	"path/filepath"

	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

// Panel fences status at machine scope. Individual backends keep independent
// ledger generations, so reporting their raw generations breaks mode changes.
func (c *Client) mapCoreStatus(kind string, request *protocol.StatusRequest) error {
	if request.CoreGeneration == 0 {
		return nil
	}
	if request.CoreGeneration >= 1<<63-1 || !security.IsUUIDv4(request.CoreInstanceID) {
		return errors.New("invalid Core status identity")
	}
	next := c.identity
	if next.StatusGeneration == 0 {
		floor := request.CoreGeneration
		for _, name := range []string{filepath.Join("core", "state.json"), "hysteria-state.json"} {
			path := filepath.Join(c.store.dataDir, name)
			var record struct {
				CoreGeneration uint64 `json:"core_generation"`
				Generation     uint64
			}
			if _, err := readModeJournal(path, &record); err != nil {
				return err
			}
			floor = max(floor, record.CoreGeneration, record.Generation)
		}
		if floor >= 1<<63-1 {
			return errors.New("Core status generation exhausted")
		}
		next.StatusGeneration = floor + 1
	} else if next.StatusCoreKind != kind || next.StatusCoreGeneration != request.CoreGeneration || next.StatusCoreInstanceID != request.CoreInstanceID {
		if next.StatusGeneration >= 1<<63-1 {
			return errors.New("Core status generation exhausted")
		}
		next.StatusGeneration = max(next.StatusGeneration+1, request.CoreGeneration+1)
	}
	if next.StatusGeneration > 1<<63-1 {
		return errors.New("Core status generation exhausted")
	}
	next.StatusCoreKind, next.StatusCoreGeneration, next.StatusCoreInstanceID = kind, request.CoreGeneration, request.CoreInstanceID
	if next.StatusGeneration != c.identity.StatusGeneration || next.StatusCoreKind != c.identity.StatusCoreKind || next.StatusCoreGeneration != c.identity.StatusCoreGeneration || next.StatusCoreInstanceID != c.identity.StatusCoreInstanceID {
		if err := c.store.Save(next); err != nil {
			return err
		}
		c.identity = next
	}
	request.CoreGeneration = next.StatusGeneration
	return nil
}
