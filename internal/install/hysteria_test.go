package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialHysteriaInstallOwnershipReinstallAndPurge(t *testing.T) {
	binary := os.Getenv("PANEL_AGENT_TEST_HYSTERIA_FILE")
	if binary == "" {
		t.Skip("PANEL_AGENT_TEST_HYSTERIA_FILE not set")
	}
	for _, owner := range []CoreOwner{CoreProject, CoreNone} {
		t.Run(string(owner), func(t *testing.T) {
			root := t.TempDir()
			program := filepath.Join(root, "agent-source")
			xray := filepath.Join(root, "xray-source")
			if err := os.WriteFile(program, []byte("agent-fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(xray, []byte("xray-fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			o := Options{Role: RoleAgent, ProjectRoot: filepath.Join(root, "opt"), DataRoot: filepath.Join(root, "data"), LogRoot: filepath.Join(root, "log"), SystemdDir: filepath.Join(root, "units"), AgentBinary: program, XrayBinary: xray, HysteriaBinary: binary, CoreOwner: owner, ServiceUser: "panel-agent", ServiceGroup: "panel-agent"}
			if owner == CoreNone {
				o.XrayBinary = ""
			}
			result, err := Install(o)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Manifests) != 1 || result.Manifests[0].Hysteria == nil || result.Manifests[0].Hysteria.Path != RuntimeHysteriaPath(o) {
				t.Fatal("Hysteria ownership not recorded")
			}
			unit, err := os.ReadFile(filepath.Join(o.SystemdDir, "panel-agent-agent.service"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(unit), "--hysteria-path") || strings.Contains(string(unit), "--xray-path") != (owner == CoreProject) {
				t.Fatalf("wrong service mode: %s", unit)
			}
			state := filepath.Join(o.DataRoot, "agent", "hysteria-state.json")
			if err := os.WriteFile(state, []byte("retained-config"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(o); err != nil {
				t.Fatal(err)
			}
			if _, err := Uninstall(o, false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(RuntimeHysteriaPath(o)); !os.IsNotExist(err) {
				t.Fatal("owned binary survived ordinary uninstall")
			}
			if data, err := os.ReadFile(state); err != nil || string(data) != "retained-config" {
				t.Fatal("ordinary uninstall deleted configuration")
			}
			if _, err := Install(o); err != nil {
				t.Fatal(err)
			}
			if _, err := Uninstall(o, true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("explicit purge retained owned data")
			}
			if _, err := os.Stat(binary); err != nil {
				t.Fatal("uninstall deleted original official source")
			}
		})
	}
}
