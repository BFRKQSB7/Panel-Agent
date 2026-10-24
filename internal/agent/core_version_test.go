package agent

import "testing"

func TestCoreVersionValidationRejectsMalformedAndOverflowValues(t *testing.T) {
	for _, value := range []string{"", "26.10", "26.10.foo", "4294967296.0.0", "26.10.21.1"} {
		if coreVersionSupported(value) || validateTargetCoreVersion(value) == nil {
			t.Fatalf("invalid Core version accepted: %q", value)
		}
	}
	for _, value := range []string{"26.10.21", "26.10.22", "27.0.0"} {
		if !coreVersionSupported(value) || validateTargetCoreVersion(value) != nil {
			t.Fatalf("supported Core version rejected: %q", value)
		}
	}
}
