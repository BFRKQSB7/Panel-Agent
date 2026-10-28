package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/install"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：panel-agent-installer install | uninstall | cleanup")
	}
	switch args[0] {
	case "install":
		return runInstall(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "cleanup":
		return runCleanup(args[1:])
	default:
		return errors.New("用法：panel-agent-installer install | uninstall | cleanup")
	}
}

type commonFlags struct {
	role           string
	projectRoot    string
	dataRoot       string
	logRoot        string
	systemdDir     string
	controlBinary  string
	agentBinary    string
	xrayBinary     string
	hysteriaBinary string
	coreOwner      string
	serviceUser    string
	serviceGroup   string
	controlListen  string
}

func (f *commonFlags) bind(flags *flag.FlagSet, role install.Role) {
	defaults := install.DefaultOptions(role)
	flags.StringVar(&f.role, "role", string(role), "control, agent, or panel-agent")
	flags.StringVar(&f.projectRoot, "project-root", defaults.ProjectRoot, "project program root")
	flags.StringVar(&f.dataRoot, "data-root", defaults.DataRoot, "project data root")
	flags.StringVar(&f.logRoot, "log-root", defaults.LogRoot, "project log root")
	flags.StringVar(&f.systemdDir, "systemd-dir", defaults.SystemdDir, "systemd unit directory")
	flags.StringVar(&f.controlBinary, "control-bin", "", "verified control binary source")
	flags.StringVar(&f.agentBinary, "agent-bin", "", "verified control-agent binary source")
	flags.StringVar(&f.xrayBinary, "xray-bin", "", "verified Xray binary source or external path")
	flags.StringVar(&f.hysteriaBinary, "hysteria-bin", "", "official Hysteria v2.13.0 binary source")
	flags.StringVar(&f.coreOwner, "core-owner", string(defaults.CoreOwner), "project or external")
	flags.StringVar(&f.serviceUser, "service-user", defaults.ServiceUser, "system service user")
	flags.StringVar(&f.serviceGroup, "service-group", defaults.ServiceGroup, "system service group")
	flags.StringVar(&f.controlListen, "control-listen", defaults.ControlListen, "Panel loopback listen address")
}

func (f commonFlags) options() (install.Options, error) {
	options := install.Options{
		Role: install.Role(f.role), ProjectRoot: f.projectRoot, DataRoot: f.dataRoot, LogRoot: f.logRoot, SystemdDir: f.systemdDir,
		ControlBinary: f.controlBinary, AgentBinary: f.agentBinary, XrayBinary: f.xrayBinary, HysteriaBinary: f.hysteriaBinary, CoreOwner: install.CoreOwner(f.coreOwner),
		ServiceUser: f.serviceUser, ServiceGroup: f.serviceGroup, ControlListen: f.controlListen,
	}
	if options.HysteriaBinary != "" && options.XrayBinary == "" {
		options.CoreOwner = install.CoreNone
	}
	return install.NormalizeOptions(options)
}

func runInstall(args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("安装器只支持 Linux systemd；当前平台仅可运行开发测试")
	}
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var common commonFlags
	common.bind(flags, install.RolePanelAgent)
	start := flags.Bool("start", false, "安装后立即启动已启用的服务；Agent 必须已经配对")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：panel-agent-installer install --role control|agent|panel-agent --control-bin PATH --agent-bin PATH --xray-bin PATH")
	}
	options, err := common.options()
	if err != nil {
		return err
	}
	if options.Role == install.RoleControl {
		if options.ControlBinary == "" {
			return errors.New("control-only 安装需要 --control-bin")
		}
	} else if options.Role == install.RoleAgent {
		if options.AgentBinary == "" || options.XrayBinary == "" && options.HysteriaBinary == "" {
			return errors.New("agent-only 安装需要 --agent-bin 及 --xray-bin 或 --hysteria-bin")
		}
	} else if options.Role == install.RolePanelAgent && (options.ControlBinary == "" || options.AgentBinary == "" || options.XrayBinary == "" && options.HysteriaBinary == "") {
		return errors.New("Panel+Agent 安装需要 --control-bin、--agent-bin 和至少一个内核")
	}
	if err := ensureServiceAccount(options.ServiceUser, options.ServiceGroup, options.DataRoot); err != nil {
		return err
	}
	result, err := install.Install(options)
	if err != nil {
		return err
	}
	if err := chownServiceRoots(options, result.Manifests...); err != nil {
		return err
	}
	names, _ := install.ServiceNames(options.Role)
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	for _, name := range names {
		if err := systemctl("enable", name); err != nil {
			return err
		}
		if *start {
			if err := systemctl("start", name); err != nil {
				return err
			}
		}
	}
	for _, manifest := range result.Manifests {
		fmt.Fprintf(os.Stdout, "已安装 %s：数据=%s\n", manifest.Role, manifest.DataDir)
	}
	return nil
}

func runUninstall(args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("安装器只支持 Linux systemd；当前平台仅可运行开发测试")
	}
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var common commonFlags
	common.bind(flags, install.RolePanelAgent)
	purge := flags.Bool("purge", false, "删除该角色自己的数据；不可恢复")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：panel-agent-installer uninstall --role control|agent|panel-agent [--purge]")
	}
	options, err := common.options()
	if err != nil {
		return err
	}
	names, err := install.OwnedServiceNames(options)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := systemctl("disable", "--now", name); err != nil {
			return err
		}
	}
	result, err := install.Uninstall(options, *purge)
	if err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	for _, path := range result.Removed {
		fmt.Fprintf(os.Stdout, "已移除：%s\n", path)
	}
	if !*purge {
		fmt.Fprintln(os.Stdout, "数据库、密钥、配置、账本和日志已保留；需要彻底删除时显式使用 --purge。")
	}
	return nil
}

func runCleanup(args []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var common commonFlags
	common.bind(flags, install.RolePanelAgent)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("用法：panel-agent-installer cleanup --role control|agent|panel-agent")
	}
	options, err := common.options()
	if err != nil {
		return err
	}
	for _, role := range []install.Role{install.RoleControl, install.RoleAgent} {
		if options.Role != install.RolePanelAgent && options.Role != role {
			continue
		}
		dataDir, err := install.DataDir(options, role)
		if err != nil {
			return err
		}
		result, err := install.Cleanup(dataDir, options.LogRoot, time.Now().UTC())
		if err != nil {
			return err
		}
		for _, path := range result.Removed {
			fmt.Fprintf(os.Stdout, "清理可再生残留：%s\n", path)
		}
		for _, path := range result.Rotated {
			fmt.Fprintf(os.Stdout, "轮转日志：%s\n", path)
		}
	}
	return nil
}

func ensureServiceAccount(serviceUser, serviceGroup, dataRoot string) error {
	if _, err := user.Lookup(serviceUser); err == nil {
		if _, err := user.LookupGroup(serviceGroup); err == nil {
			return nil
		}
	}
	if serviceUser != serviceGroup {
		return errors.New("service user/group must already exist when they differ")
	}
	command := exec.Command("useradd", "--system", "--home-dir", dataRoot, "--shell", "/usr/sbin/nologin", "--user-group", serviceUser)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create service account: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func chownServiceRoots(options install.Options, manifests ...install.Manifest) error {
	paths := []string{options.DataRoot, options.LogRoot}
	for _, role := range []install.Role{install.RoleControl, install.RoleAgent} {
		if options.Role != install.RolePanelAgent && options.Role != role {
			continue
		}
		dataDir, err := install.DataDir(options, role)
		if err != nil {
			return err
		}
		paths = append(paths, dataDir)
		paths = append(paths, dataDir+string(os.PathSeparator)+"ownership.json")
		if role == install.RoleAgent && options.CoreOwner == install.CoreProject {
			paths = append(paths, filepath.Dir(install.RuntimeCorePath(options)), install.RuntimeCorePath(options))
		}
		paths = append(paths, options.LogRoot+string(os.PathSeparator)+string(role)+".log")
	}
	for _, manifest := range manifests {
		if manifest.Hysteria != nil {
			paths = append(paths, filepath.Dir(manifest.Hysteria.Path), manifest.Hysteria.Path)
		}
	}
	for _, path := range paths {
		command := exec.Command("chown", options.ServiceUser+":"+options.ServiceGroup, path)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("set ownership for %s: %s: %w", path, strings.TrimSpace(string(output)), err)
		}
	}
	return nil
}

func systemctl(args ...string) error {
	command := exec.Command("systemctl", args...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
	return nil
}
