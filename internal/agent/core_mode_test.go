package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHysteriaMustApplyRemovalBeforeOmittingCore(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		blocked    bool
	}{
		{"enabled", `{"Version":1,"Desired":{"nodes":[{"enabled":true,"extension":{"kind":"hysteria2_tls"}}]}}`, true},
		{"pending", `{"Version":1,"Pending":true}`, true},
		{"disabled", `{"Version":1,"Desired":{"nodes":[{"enabled":false,"extension":{"kind":"hysteria2_tls"}}]}}`, false},
		{"removed", `{"Version":1,"Desired":{"nodes":[]}}`, false},
		{"corrupt", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "hysteria-state.json"), []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := requireHysteriaRemoval(dir); (err != nil) != tc.blocked {
				t.Fatalf("blocked=%v error=%v", tc.blocked, err)
			}
		})
	}
}
