package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"panel-agent.local/panel-agent/internal/agent"
	"panel-agent.local/panel-agent/internal/agent/ledger"
	"panel-agent.local/panel-agent/internal/install"
	"panel-agent.local/panel-agent/internal/protocol"
	"panel-agent.local/panel-agent/internal/security"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：control-agent pair | re-pair | sync | run | ledger | upgrade | update | check | version | cleanup")
	}
	switch args[0] {
	case "version":
		if len(args) != 1 {
			return errors.New("用法：control-agent version")
		}
		fmt.Fprintf(os.Stdout, "Control-Agent %s\n", protocol.AgentVersion)
		return nil
	case "check":
		flags := flag.NewFlagSet("check", flag.ContinueOnError)
		dir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
		required := flags.Int("require-ledger-schema", 0, "minimum ledger reader version required by the updater")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("用法：control-agent check [--data-dir PATH]")
		}
		if *required < 0 || *required > 2 {
			return errors.New("此程序不支持所需的账本格式")
		}
		return agent.CheckProgramData(context.Background(), *dir)
	case "update":
		flags := flag.NewFlagSet("update", flag.ContinueOnError)
		dir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
		candidate := flags.String("package", "", "local Agent executable")
		version := flags.String("target-version", "", "target Agent version")
		digest := flags.String("sha256", "", "Agent SHA-256")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *candidate == "" || *version == "" || *digest == "" {
			return errors.New("用法：control-agent update --package PATH --target-version VERSION --sha256 SHA256 [--data-dir PATH]")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return agent.UpdateProgram(context.Background(), *dir, executable, *candidate, *version, *digest)
	case "pair":
		return runPair(args[1:], false)
	case "re-pair":
		return runPair(args[1:], true)
	case "sync":
		return runSync(args[1:])
	case "upgrade-hysteria":
		return runHysteriaUpgrade(args[1:])
	case "run":
		return runAgent(args[1:])
	case "ledger":
		flags := flag.NewFlagSet("ledger", flag.ContinueOnError)
		dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("用法：control-agent ledger [--data-dir PATH]")
		}
		diagnostic, err := ledger.Inspect(context.Background(), *dataDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(diagnostic)
	case "upgrade":
		return runUpgrade(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	default:
		return errors.New("用法：control-agent pair | re-pair | sync | run | ledger | upgrade | update | check | version | cleanup")
	}
}

func runUpgrade(args []string) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	caCert := flags.String("ca-cert", "", "additional PEM CA certificate")
	xrayPath := flags.String("xray-path", "", "absolute path to the project-owned Xray binary")
	packagePath := flags.String("package", "", "local official Xray ZIP or executable")
	targetVersion := flags.String("target-version", "", "target Xray version")
	packageSHA256 := flags.String("sha256", "", "SHA-256 of the package")
	developmentLoopback := flags.Bool("development-loopback-test", false, "allow pure development Nodes to reach only TCP 127.0.0.1:18080 and UDP 127.0.0.1:18081")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *xrayPath == "" || *packagePath == "" || *targetVersion == "" || *packageSHA256 == "" {
		return errors.New("用法：control-agent upgrade --xray-path PATH --package PATH --target-version VERSION --sha256 SHA256 [--data-dir PATH] [--ca-cert PEM]")
	}
	client, err := agent.LoadClient(*dataDir, *caCert)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.EnableCore(context.Background(), *xrayPath, *developmentLoopback); err != nil {
		return err
	}
	intentID, err := security.NewUUID()
	if err != nil {
		return err
	}
	outcome, err := client.UpgradePackage(context.Background(), intentID, *packagePath, *targetVersion, *packageSHA256)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Xray 已升级到 %s，Core generation=%d。\n", outcome.ActualVersion, outcome.Generation)
	return nil
}

func runCleanup(args []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", install.DefaultDataRoot+"/agent", "Agent data directory")
	logRoot := flags.String("log-root", install.DefaultLogRoot, "Agent log directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent cleanup [--data-dir PATH] [--log-root PATH]")
	}
	result, err := install.Cleanup(*dataDir, *logRoot, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "清理完成：残留 %d 项，轮转日志 %d 项。\n", len(result.Removed), len(result.Rotated))
	return nil
}

func runPair(args []string, repair bool) error {
	command := "pair"
	if repair {
		command = "re-pair"
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	panelURL := flags.String("panel-url", "", "Panel HTTPS origin")
	displayName := flags.String("name", "", "machine display name")
	caCert := flags.String("ca-cert", "", "additional PEM CA certificate")
	joinTokenStdin := flags.Bool("join-token-stdin", false, "read Join Token from stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent pair [--panel-url HTTPS_URL] [--data-dir PATH] [--join-token-stdin]")
	}
	joinToken := ""
	var err error
	if repair {
		joinToken, err = readJoinToken(*joinTokenStdin)
		if err != nil {
			return err
		}
	} else {
		pending, err := agent.HasPendingPairing(*dataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !pending {
			joinToken, err = readJoinToken(*joinTokenStdin)
			if err != nil {
				return err
			}
		}
	}
	ctx := context.Background()
	machineID, err := agent.Pair(ctx, *dataDir, strings.TrimSpace(*panelURL), *displayName, joinToken, *caCert, repair)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Agent 已配对。机器 ID：%s\n", machineID)
	return nil
}

func readJoinToken(stdin bool) (string, error) {
	if stdin {
		reader := bufio.NewReaderSize(os.Stdin, 128)
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return "", errors.New("标准输入必须包含一行以换行结束的 Join Token")
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
			return "", errors.New("标准输入只能包含一行 Join Token")
		}
		if !security.IsValidToken(value, security.MachineSecretBytes) {
			return "", errors.New("Join Token 无效")
		}
		return value, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("当前输入不是交互终端，请使用 --join-token-stdin")
	}
	fmt.Fprint(os.Stderr, "输入一次性 Join Token：")
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取 Join Token 失败：%w", err)
	}
	defer clear(value)
	token := string(value)
	if !security.IsValidToken(token, security.MachineSecretBytes) {
		return "", errors.New("Join Token 无效")
	}
	return token, nil
}

func runSync(args []string) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	caCert := flags.String("ca-cert", "", "additional PEM CA certificate")
	xrayPath := flags.String("xray-path", "", "absolute path to the verified supported Xray binary")
	hysteriaPath := flags.String("hysteria-path", "", "absolute path to official Hysteria v2.13.0")
	developmentLoopback := flags.Bool("development-loopback-test", false, "allow pure development Nodes to reach only TCP 127.0.0.1:18080 and UDP 127.0.0.1:18081")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent sync [--data-dir PATH] [--ca-cert PEM] [--xray-path PATH]")
	}
	client, err := agent.LoadClient(*dataDir, *caCert)
	if err != nil {
		return err
	}
	defer client.Close()
	if *xrayPath != "" || *hysteriaPath != "" {
		if err := client.EnableCores(context.Background(), *xrayPath, *hysteriaPath, *developmentLoopback); err != nil {
			return err
		}
	}
	if err := client.SyncOnce(context.Background()); err != nil {
		return err
	}
	if *xrayPath == "" && *hysteriaPath == "" {
		fmt.Fprintln(os.Stdout, "已同步配置并上报状态；未启用 Core 管理，未报告 applied 成功。")
	} else {
		fmt.Fprintln(os.Stdout, "已同步配置、完成 Core 应用与健康检查并上报状态。")
	}
	return nil
}

func runHysteriaUpgrade(args []string) error {
	flags := flag.NewFlagSet("upgrade-hysteria", flag.ContinueOnError)
	dir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	binary := flags.String("hysteria-path", "", "project-owned official Hysteria executable")
	candidate := flags.String("package", "", "local verified official executable")
	version := flags.String("target-version", "", "target Hysteria version")
	digest := flags.String("sha256", "", "official asset SHA256")
	ca := flags.String("ca-cert", "", "additional Panel CA")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *binary == "" || *candidate == "" || *version == "" || *digest == "" {
		return errors.New("用法：control-agent upgrade-hysteria --hysteria-path PATH --package PATH --target-version VERSION --sha256 SHA256 [--data-dir PATH]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := agent.LoadClient(*dir, *ca)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.EnableCores(ctx, "", *binary, false); err != nil {
		return err
	}
	return client.UpgradeHysteria(ctx, *candidate, *version, *digest)
}

func runAgent(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	caCert := flags.String("ca-cert", "", "additional PEM CA certificate")
	xrayPath := flags.String("xray-path", "", "absolute path to the verified supported Xray binary")
	hysteriaPath := flags.String("hysteria-path", "", "absolute path to official Hysteria v2.13.0")
	developmentLoopback := flags.Bool("development-loopback-test", false, "allow pure development Nodes to reach only TCP 127.0.0.1:18080 and UDP 127.0.0.1:18081")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent run [--data-dir PATH] [--ca-cert PEM] [--xray-path PATH]")
	}
	client, err := agent.LoadClient(*dataDir, *caCert)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *xrayPath != "" || *hysteriaPath != "" {
		if err := client.EnableCores(ctx, *xrayPath, *hysteriaPath, *developmentLoopback); err != nil {
			return err
		}
	}
	if err := client.Run(ctx); errors.Is(err, context.Canceled) {
		return nil
	} else {
		return err
	}
}

func defaultDataDir() string {
	if value := os.Getenv("PANEL_AGENT_DATA_DIR"); value != "" {
		return value
	}
	return "./agent-data"
}
