package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallIsIdempotentAndUninstallHasTwoLevels(t *testing.T) {
	root := t.TempDir()
	sources := filepath.Join(root, "sources")
	project := filepath.Join(root, "opt")
	data := filepath.Join(root, "var", "lib")
	logs := filepath.Join(root, "var", "log")
	units := filepath.Join(root, "etc", "systemd", "system")
	if err := os.MkdirAll(sources, 0o700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(sources, "control")
	agent := filepath.Join(sources, "control-agent")
	xray := filepath.Join(sources, "xray")
	for _, path := range []string{control, agent, xray} {
		if err := os.WriteFile(path, []byte("binary-"+filepath.Base(path)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Role: RolePanelAgent, ProjectRoot: project, DataRoot: data, LogRoot: logs, SystemdDir: units, ControlBinary: control, AgentBinary: agent, XrayBinary: xray, CoreOwner: CoreProject, ServiceUser: "panel-agent", ServiceGroup: "panel-agent"}
	if _, err := Install(options); err != nil {
		t.Fatal(err)
	}
	if names, err := OwnedServiceNames(options); err != nil || len(names) != 6 {
		t.Fatalf("owned service discovery = %v, %v", names, err)
	}
	if _, err := Install(options); err != nil {
		t.Fatalf("idempotent install failed: %v", err)
	}
	for _, path := range []string{
		filepath.Join(project, "bin", "control"), filepath.Join(project, "bin", "control-agent"), RuntimeCorePath(options),
		filepath.Join(units, "panel-agent-control.service"), filepath.Join(units, "panel-agent-agent.service"),
		filepath.Join(data, "control", "ownership.json"), filepath.Join(data, "agent", "ownership.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("installed path missing %s: %v", path, err)
		}
	}
	if unit, err := os.ReadFile(filepath.Join(units, "panel-agent-agent.service")); err != nil || !strings.Contains(string(unit), systemdQuote(RuntimeCorePath(options))) || strings.Contains(string(unit), systemdQuote(xray)) || !strings.Contains(string(unit), "ProtectSystem=strict") {
		t.Fatalf("agent unit missing ownership boundaries: %v %s", err, unit)
	}
	if err := os.WriteFile(filepath.Join(data, "agent", "agent.ledger.sqlite"), []byte("unacked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(options, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(data, "agent", "ownership.json"), filepath.Join(data, "agent", "agent.ledger.sqlite"), filepath.Join(logs, "agent.log")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ordinary uninstall removed retained data %s: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(project, "bin", "control"), filepath.Join(project, "bin", "control-agent"), filepath.Join(units, "panel-agent-control.service")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("ordinary uninstall retained program path %s: %v", path, err)
		}
	}
	if _, err := Uninstall(options, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(data, "control"), filepath.Join(data, "agent"), filepath.Join(logs, "control.log"), filepath.Join(logs, "agent.log")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("purge retained role data %s: %v", path, err)
		}
	}
	if _, err := os.Stat(xray); err != nil {
		t.Fatalf("purge touched source outside project roots: %v", err)
	}
}

func TestExternalCoreAndSymlinkAreNeverFollowed(t *testing.T) {
	root := t.TempDir()
	sources := filepath.Join(root, "sources")
	if err := os.MkdirAll(sources, 0o700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(sources, "control")
	agent := filepath.Join(sources, "control-agent")
	external := filepath.Join(root, "external-xray")
	for _, path := range []string{control, agent, external} {
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Role: RoleAgent, ProjectRoot: filepath.Join(root, "opt"), DataRoot: filepath.Join(root, "data"), LogRoot: filepath.Join(root, "logs"), SystemdDir: filepath.Join(root, "units"), ControlBinary: control, AgentBinary: agent, XrayBinary: external, CoreOwner: CoreExternal, ServiceUser: "panel-agent", ServiceGroup: "panel-agent"}
	if _, err := Install(options); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(options.SystemdDir, "panel-agent-agent.service")
	if err := os.Remove(unitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, unitPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := OwnedServiceNames(options); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked unit was not rejected before systemd control: %v", err)
	}
	if _, err := Uninstall(options, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external Core target was removed through an entry link: %v", err)
	}
	if _, err := os.Lstat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("owned entry link was not unlinked: %v", err)
	}
}

func TestCleanupRotatesKnownLogsAndPreservesSecurityState(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent")
	logRoot := filepath.Join(root, "logs")
	if err := os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "debug"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "core"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "tmp", "candidate.tmp"), []byte("recreate"), 0o600); err != nil {
		t.Fatal(err)
	}
	debug := filepath.Join(dataDir, "debug", "debug-old.log")
	if err := os.WriteFile(debug, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(debug, old, old); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(logRoot, "agent.log"), filepath.Join(dataDir, "core", "core.log")} {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxCleanupLogBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(dataDir, "agent.ledger.sqlite"), filepath.Join(dataDir, "identity.key"), filepath.Join(dataDir, "core", "state.json")} {
		if err := os.WriteFile(path, []byte("must-keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Cleanup(dataDir, logRoot, time.Now())
	if err != nil || len(result.Rotated) != 2 {
		t.Fatalf("cleanup result = %+v, err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tmp", "candidate.tmp")); !os.IsNotExist(err) {
		t.Fatalf("regenerable temp survived: %v", err)
	}
	if _, err := os.Stat(debug); !os.IsNotExist(err) {
		t.Fatalf("expired debug file survived: %v", err)
	}
	for _, path := range []string{filepath.Join(dataDir, "agent.ledger.sqlite"), filepath.Join(dataDir, "identity.key"), filepath.Join(dataDir, "core", "state.json"), filepath.Join(logRoot, "agent.log.1"), filepath.Join(dataDir, "core", "core.log.1")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup removed required or rotated path %s: %v", path, err)
		}
	}
}
