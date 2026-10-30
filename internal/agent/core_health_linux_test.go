package agent

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	command "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

type healthOnlyStats struct {
	command.UnimplementedStatsServiceServer
}

func (healthOnlyStats) QueryStats(_ context.Context, r *command.QueryStatsRequest) (*command.QueryStatsResponse, error) {
	if r.GetReset_() {
		os.Exit(72)
	}
	return &command.QueryStatsResponse{}, nil
}

func init() {
	if os.Getenv("PANEL_AGENT_HEALTH_ONLY_HELPER") != "1" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:10085")
	if err != nil {
		os.Exit(71)
	}
	server := grpc.NewServer()
	command.RegisterStatsServiceServer(server, healthOnlyStats{})
	// GetSysStats deliberately remains unavailable. Accounting stays responsive.
	if server.Serve(listener) != nil {
		os.Exit(73)
	}
	os.Exit(0)
}

func TestHealthUsesAccountingWithoutRuntimeMemoryStatistics(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &CoreManager{binary: executable, dir: t.TempDir()}
	record := &coreConfigRecord{ConfigHash: strings.Repeat("a", 64), Listeners: []string{"127.0.0.1:10085"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "run", "-config", m.configPath(record))
	child.Env = append(os.Environ(), "PANEL_AGENT_HEALTH_ONLY_HELPER=1")
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	var process coreProcess
	for attempt := 0; attempt < 50; attempt++ {
		process, err = inspectCoreProcess(child.Process.Pid, executable)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = m.health(ctx, process, record, true); err != nil {
		t.Fatalf("responsive accounting must not require heap statistics: %v", err)
	}
	if time.Since(started) < time.Second {
		t.Fatal("startup confirmation window was skipped")
	}
	if actual, e := inspectCoreProcess(child.Process.Pid, executable); e != nil || actual != process {
		t.Fatal("health probe changed the running Core")
	}
}
