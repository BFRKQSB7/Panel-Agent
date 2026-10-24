//go:build !linux

package agent

import (
	"errors"
)

func availableDiskBytes(string) (uint64, error) {
	return 0, errors.New("Core upgrade disk checks require Linux")
}
