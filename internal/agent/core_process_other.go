//go:build !linux

package agent

import (
	"context"
	"errors"
)

const managedCoreSupported = false

type coreProcess struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"start_ticks"`
	ConfigPath string `json:"config_path"`
	BootID     string `json:"boot_id,omitempty"`
}

func inspectCoreProcess(int, string) (coreProcess, error) {
	return coreProcess{}, errors.New("managed Core requires Linux")
}

func findCoreProcesses(string, map[string]bool) ([]coreProcess, error) {
	return nil, errors.New("managed Core requires Linux")
}

func stopCoreProcess(context.Context, coreProcess, string) error {
	return errors.New("managed Core requires Linux")
}

func startCoreCommand(string, string) (coreProcess, error) {
	return coreProcess{}, errors.New("managed Core requires Linux")
}
