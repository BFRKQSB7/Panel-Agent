package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const minimumSupportedCoreVersion = "26.10.21"

var xrayVersionPattern = regexp.MustCompile(`(?m)^Xray ([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)`)
var coreVersionValuePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func readCoreVersion(ctx context.Context, binary string) (string, error) {
	output := &limitedCoreOutput{limit: 4096}
	command := exec.CommandContext(ctx, binary, "version")
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		return "", err
	}
	match := xrayVersionPattern.FindStringSubmatch(output.String())
	if len(match) != 2 || !coreVersionSupported(match[1]) {
		return "", errors.New("Core 版本不在受支持范围")
	}
	return match[1], nil
}

func coreVersionSupported(value string) bool {
	left, ok := parseCoreVersion(value)
	if !ok {
		return false
	}
	right, ok := parseCoreVersion(minimumSupportedCoreVersion)
	if !ok {
		return false
	}
	return compareParsedCoreVersions(left, right) >= 0
}

func compareCoreVersions(left, right string) int {
	leftParsed, leftOK := parseCoreVersion(left)
	rightParsed, rightOK := parseCoreVersion(right)
	if !leftOK || !rightOK {
		return 0
	}
	return compareParsedCoreVersions(leftParsed, rightParsed)
}

func parseCoreVersion(value string) ([3]uint64, bool) {
	var parsed [3]uint64
	if !coreVersionValuePattern.MatchString(value) {
		return parsed, false
	}
	parts := strings.Split(value, ".")
	for i := range parts {
		if len(parts[i]) > 1 && parts[i][0] == '0' {
			return [3]uint64{}, false
		}
		component, err := strconv.ParseUint(parts[i], 10, 32)
		if err != nil {
			return [3]uint64{}, false
		}
		parsed[i] = component
	}
	return parsed, true
}

func compareParsedCoreVersions(left, right [3]uint64) int {
	for i := range left {
		leftValue, rightValue := left[i], right[i]
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func validateTargetCoreVersion(value string) error {
	if len([]byte(value)) > 32 || !coreVersionSupported(value) {
		return fmt.Errorf("Core 版本不受支持：%s", value)
	}
	return nil
}
