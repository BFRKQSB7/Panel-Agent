package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"panel-agent.local/panel-agent/internal/control"
	"panel-agent.local/panel-agent/internal/install"
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
		return errors.New("用法：control admin init | control serve | control cleanup")
	}
	switch args[0] {
	case "admin":
		if len(args) < 2 || args[1] != "init" {
			return errors.New("用法：control admin init [--data-dir PATH] [--password-stdin]")
		}
		return runAdminInit(args[2:])
	case "serve":
		return runServe(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	default:
		return errors.New("用法：control admin init | control serve | control cleanup")
	}
}

func runCleanup(args []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", install.DefaultDataRoot+"/control", "Panel data directory")
	logRoot := flags.String("log-root", install.DefaultLogRoot, "Panel log directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control cleanup [--data-dir PATH] [--log-root PATH]")
	}
	result, err := install.Cleanup(*dataDir, *logRoot, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "清理完成：残留 %d 项，轮转日志 %d 项。\n", len(result.Removed), len(result.Rotated))
	return nil
}

func runAdminInit(args []string) error {
	flags := flag.NewFlagSet("admin init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", defaultDataDir(), "Panel data directory")
	passwordStdin := flags.Bool("password-stdin", false, "read two password lines from stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control admin init [--data-dir PATH] [--password-stdin]")
	}
	password, confirmation, err := readAdminPasswords(*passwordStdin)
	if err != nil {
		return err
	}
	defer clear(password)
	defer clear(confirmation)
	if len(password) != len(confirmation) || subtle.ConstantTimeCompare(password, confirmation) != 1 {
		return errors.New("password confirmation did not match")
	}
	passwordHash, err := security.HashAdminPassword(password)
	if err != nil {
		return err
	}
	clear(password)
	installationID, err := control.InitializeAdmin(*dataDir, passwordHash)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "管理员已初始化。安装 ID：%s\n", installationID)
	return nil
}

func readAdminPasswords(passwordStdin bool) ([]byte, []byte, error) {
	if passwordStdin {
		reader := bufio.NewReaderSize(os.Stdin, 128)
		first, err := readPasswordLine(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("read password input: %w", err)
		}
		second, err := readPasswordLine(reader)
		if err != nil {
			clear(first)
			return nil, nil, fmt.Errorf("read password confirmation: %w", err)
		}
		if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
			clear(first)
			clear(second)
			if err == nil {
				return nil, nil, errors.New("password input must contain exactly two lines")
			}
			return nil, nil, fmt.Errorf("finish reading password input: %w", err)
		}
		return first, second, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, nil, errors.New("当前输入不是交互终端，请使用 --password-stdin")
	}
	fmt.Fprint(os.Stderr, "Administrator password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("read administrator password: %w", err)
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		clear(first)
		return nil, nil, fmt.Errorf("read password confirmation: %w", err)
	}
	return first, second, nil
}

func readPasswordLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, errors.New("each password must be a newline-terminated line")
	}
	password := strings.TrimSuffix(string(line), "\n")
	password = strings.TrimSuffix(password, "\r")
	if len(password) > 72 {
		return nil, security.ErrInvalidAdminPassword
	}
	return []byte(password), nil
}

func runServe(args []string) error {
	config, err := control.ConfigFromEnvironment()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&config.DataDir, "data-dir", config.DataDir, "Panel data directory")
	flags.StringVar(&config.Listen, "listen", config.Listen, "management listen address")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：control serve [--data-dir PATH] [--listen HOST:PORT]")
	}
	server, err := control.NewServer(config)
	if err != nil {
		return err
	}
	defer server.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return server.Serve(ctx)
}

func defaultDataDir() string {
	if value := os.Getenv("PANEL_DATA_DIR"); value != "" {
		return value
	}
	return "./data"
}
