package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/period"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
	"panel-agent.local/panel-agent/internal/storage"
)

func TestAgentProgramUpdatePreservesIdentityAndUnacknowledgedLedger(t *testing.T) {
	for _, scenario := range []string{"success", "check-failure", "legacy-reader", "health-failure", "crash-swapped", "crash-committed", "busy", "real-program"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "real-program" && os.Getenv("PANEL_AGENT_TEST_AGENT_PROGRAM") == "" {
				t.Skip("requires explicit built Agent executable")
			}
			root := t.TempDir()
			dir := filepath.Join(root, "agent-data")
			store, err := openIdentityStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			machine, _ := security.NewUUID()
			registration, _ := security.NewUUID()
			secret, _ := security.NewToken(security.MachineSecretBytes)
			if err := store.Save(Identity{Version: 1, MachineID: machine, RegistrationID: registration, MachineSecret: secret, PanelURL: "https://panel.example.test", DesiredState: protocol.EmptyDesiredState()}); err != nil {
				t.Fatal(err)
			}
			identityBefore, err := os.ReadFile(store.path)
			if err != nil {
				t.Fatal(err)
			}
			book, err := ledger.Open(dir, machine)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := period.New(period.Scope{Kind: "machine", MachineID: machine}, 1, time.Unix(0, 0).UTC(), period.DefaultMonthly(1))
			if err != nil {
				t.Fatal(err)
			}
			source, _ := security.NewUUID()
			now := time.Now().UTC()
			if err := book.RegisterSource(context.Background(), source, 1, now.Add(-time.Second), true); err != nil {
				t.Fatal(err)
			}
			if err := book.Ingest(context.Background(), ledger.Sample{SourceID: source, Generation: 1, Started: now.Add(-time.Millisecond), Finished: now, Policies: []period.Policy{policy}, Readings: []ledger.Reading{{Label: "transit-d15", Stream: protocol.TrafficStream{Kind: "machine_only_transit"}, Totals: protocol.Totals{Uplink: 12345, Downlink: 67890}}}}); err != nil {
				t.Fatal(err)
			}
			before, err := book.Diagnostic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			book.Close()
			if len(before.PendingPreview) != 1 {
				t.Fatal("fixture is missing unacknowledged data")
			}
			active := filepath.Join(root, "control-agent")
			old := []byte("#!/bin/sh\nprintf 'Control-Agent 0.1.0\\n'\n")
			if err := os.WriteFile(active, old, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := installAgentTestManifest(dir, active)
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.WritePrivateAtomic(filepath.Join(dir, "ownership.json"), encoded); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(root, "candidate")
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			script := []byte("#!/bin/sh\nexec " + quote(testBinary) + " -test.run '^TestAgentProgramCandidateWorker$' -- \"$0\" \"$@\"\n")
			if scenario == "real-program" {
				if err := copyUpgradeTestBinary(os.Getenv("PANEL_AGENT_TEST_AGENT_PROGRAM"), candidate); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(candidate, script, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PANEL_AGENT_D15_AGENT_SCENARIO", scenario)
			t.Setenv("PANEL_AGENT_D15_AGENT_ROOT", root)
			if scenario == "busy" {
				lock, e := storage.AcquireDataLock(dir, "agent.lock")
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Close()
			}
			if strings.HasPrefix(scenario, "crash-") {
				command := exec.Command(testBinary, "-test.run=^TestAgentProgramUpdateCrashWorker$")
				output, e := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("Agent crash worker: %v %s", e, output)
				}
				rolledBack, e := recoverAgentProgram(dir, active)
				if e != nil || rolledBack == (scenario == "crash-committed") {
					t.Fatal("Agent recovery decision", rolledBack, e)
				}
			} else {
				err = UpdateProgram(context.Background(), dir, active, candidate, "0.1.0", candidateDigest(candidate))
				if (scenario == "success" || scenario == "real-program") != (err == nil) {
					t.Fatal("Agent update outcome", err)
				}
			}
			wantCandidate := scenario == "success" || scenario == "real-program" || scenario == "crash-committed"
			if wantCandidate && candidateDigest(active) != candidateDigest(candidate) {
				t.Fatal("candidate was not installed")
			}
			actual, err := os.ReadFile(active)
			if err != nil {
				t.Fatal(err)
			}
			if !wantCandidate && !bytes.Equal(actual, old) {
				t.Fatal("old Agent was not retained")
			}
			identityAfter, err := os.ReadFile(store.path)
			if err != nil || !bytes.Equal(identityBefore, identityAfter) {
				t.Fatal("identity was changed", err)
			}
			after, err := ledger.Inspect(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(before.PendingPreview)
			afterJSON, _ := json.Marshal(after.PendingPreview)
			if before.Metadata.Epoch != after.Metadata.Epoch || !bytes.Equal(beforeJSON, afterJSON) {
				t.Fatal("unacknowledged ledger changed")
			}
			t.Logf("scenario=%s identity=unchanged epoch=unchanged unacked_segments=%d", scenario, len(after.PendingPreview))
		})
	}
}

func installAgentTestManifest(dir, binary string) map[string]any {
	return map[string]any{"version": 1, "role": "agent", "data_dir": dir, "paths": []any{map[string]any{"kind": "binary", "path": binary, "role": "agent"}}}
}

func TestAgentProgramCandidateWorker(t *testing.T) {
	scenario := os.Getenv("PANEL_AGENT_D15_AGENT_SCENARIO")
	if scenario == "" {
		t.Skip("subprocess only")
	}
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 2 {
		t.Fatal("candidate arguments missing")
	}
	if args[1] == "version" {
		fmt.Println("Control-Agent 0.1.0")
		os.Exit(0)
	}
	if scenario == "legacy-reader" {
		os.Exit(1)
	}
	if len(args) != 5 || args[1] != "check" || args[2] != "--data-dir" || args[4] != "--require-ledger-schema=2" {
		t.Fatal("unexpected check invocation")
	}
	if err := CheckProgramData(context.Background(), args[3]); err != nil {
		t.Fatal(err)
	}
	if scenario == "check-failure" || scenario == "health-failure" && args[0] == filepath.Join(os.Getenv("PANEL_AGENT_D15_AGENT_ROOT"), "control-agent") {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAgentProgramUpdateCrashWorker(t *testing.T) {
	root := os.Getenv("PANEL_AGENT_D15_AGENT_ROOT")
	scenario := os.Getenv("PANEL_AGENT_D15_AGENT_SCENARIO")
	if !strings.HasPrefix(scenario, "crash-") {
		t.Skip("subprocess only")
	}
	candidate := filepath.Join(root, "candidate")
	err := updateProgram(context.Background(), filepath.Join(root, "agent-data"), filepath.Join(root, "control-agent"), candidate, "0.1.0", candidateDigest(candidate), func(phase string) {
		if "crash-"+phase == scenario {
			os.Exit(77)
		}
	})
	t.Fatal("crash checkpoint not reached", err)
}
