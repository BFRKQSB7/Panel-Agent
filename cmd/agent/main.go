package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/term"

	"panel-agent.local/panel-agent/internal/agent"
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
		return errors.New("用法：control-agent pair | re-pair | sync | run")
	}
	switch args[0] {
	case "pair":
		return runPair(args[1:], false)
	case "re-pair":
		return runPair(args[1:], true)
	case "sync":
		return runSync(args[1:])
	case "run":
		return runAgent(args[1:])
	default:
		return errors.New("用法：control-agent pair | re-pair | sync | run")
	}
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
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent sync [--data-dir PATH] [--ca-cert PEM]")
	}
	client, err := agent.LoadClient(*dataDir, *caCert)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.SyncOnce(context.Background()); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "已同步完整 desired-state 快照并回报机器状态；Core 尚未实现，未报告 applied 成功。")
	return nil
}

func runAgent(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Agent data directory")
	caCert := flags.String("ca-cert", "", "additional PEM CA certificate")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control-agent run [--data-dir PATH] [--ca-cert PEM]")
	}
	client, err := agent.LoadClient(*dataDir, *caCert)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
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
