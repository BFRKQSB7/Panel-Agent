// Package install owns the Linux installation layout and its destructive
// boundaries. It never treats a path outside the recorded project roots as a
// project resource, and it never owns an external Core binary.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"panel-agent.local/panel-agent/internal/storage"
)

const (
	ManifestVersion      = 1
	DefaultProjectRoot   = "/opt/panel-agent"
	DefaultDataRoot      = "/var/lib/panel-agent"
	DefaultLogRoot       = "/var/log/panel-agent"
	DefaultSystemdDir    = "/etc/systemd/system"
	DefaultServiceUser   = "panel-agent"
	DefaultServiceGroup  = "panel-agent"
	DefaultControlListen = "127.0.0.1:8080"
	MaxManifestBytes     = 1 << 20
	MaxCleanupLogBytes   = 8 << 20
	ManagedUnitMarker    = "# Managed by Panel-Agent; ownership is recorded in ownership.json."
)

var (
	ErrUnsupportedRole = errors.New("不支持的安装角色")
	ErrUnownedPath     = errors.New("路径未被 Panel-Agent 所有权清单声明")
	ErrUnsafePath      = errors.New("路径不安全或包含符号链接")
	ErrManifest        = errors.New("所有权清单无效")
	serviceNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

type Role string

const (
	RoleControl    Role = "control"
	RoleAgent      Role = "agent"
	RolePanelAgent Role = "panel-agent"
)

type CoreOwner string

const (
	CoreProject  CoreOwner = "project"
	CoreExternal CoreOwner = "external"
)

type Options struct {
	Role          Role
	ProjectRoot   string
	DataRoot      string
	LogRoot       string
	SystemdDir    string
	ControlBinary string
	AgentBinary   string
	XrayBinary    string
	CoreOwner     CoreOwner
	ServiceUser   string
	ServiceGroup  string
	ControlListen string
}

type PathOwnership struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Role      Role   `json:"role"`
	Source    string `json:"source"`
	Uninstall bool   `json:"remove_on_uninstall"`
	Purge     bool   `json:"remove_on_purge"`
}

type CoreOwnership struct {
	Owner        CoreOwner `json:"owner"`
	Path         string    `json:"path,omitempty"`
	PreviousPath string    `json:"previous_path,omitempty"`
}

type Manifest struct {
	Version     int             `json:"version"`
	Role        Role            `json:"role"`
	ProjectRoot string          `json:"project_root"`
	DataDir     string          `json:"data_dir"`
	LogRoot     string          `json:"log_root"`
	SystemdDir  string          `json:"systemd_dir"`
	Core        CoreOwnership   `json:"core"`
	Paths       []PathOwnership `json:"paths"`
	InstalledAt time.Time       `json:"installed_at"`
}

type Result struct {
	Manifests []Manifest
	Removed   []string
	Kept      []string
}

func DefaultOptions(role Role) Options {
	return Options{
		Role:          role,
		ProjectRoot:   DefaultProjectRoot,
		DataRoot:      DefaultDataRoot,
		LogRoot:       DefaultLogRoot,
		SystemdDir:    DefaultSystemdDir,
		CoreOwner:     CoreProject,
		ServiceUser:   DefaultServiceUser,
		ServiceGroup:  DefaultServiceGroup,
		ControlListen: DefaultControlListen,
	}
}

func DataDir(options Options, role Role) (string, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return "", err
	}
	if role != RoleControl && role != RoleAgent {
		return "", ErrUnsupportedRole
	}
	return roleDataDir(options, role), nil
}

func ServiceNames(role Role) ([]string, error) {
	if role != RoleControl && role != RoleAgent && role != RolePanelAgent {
		return nil, ErrUnsupportedRole
	}
	names := []string{}
	for _, item := range expandRoles(role) {
		names = append(names, serviceName(item))
		names = append(names, serviceName(item)+"-cleanup.timer")
	}
	return names, nil
}

func NormalizeOptions(options Options) (Options, error) {
	return normalizeOptions(options)
}

func OwnedServiceNames(options Options) ([]string, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, role := range expandRoles(options.Role) {
		manifest, err := loadManifest(options, role)
		if err != nil {
			return nil, err
		}
		if err := validateManifest(options, role, manifest); err != nil {
			return nil, err
		}
		units := []struct{ name, path string }{
			{name: serviceName(role), path: filepath.Join(options.SystemdDir, serviceName(role)+".service")},
			{name: serviceName(role) + "-cleanup.service", path: filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.service")},
			{name: serviceName(role) + "-cleanup.timer", path: filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.timer")},
		}
		for _, unit := range units {
			unitPath := unit.path
			info, statErr := os.Lstat(unitPath)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%w: %s", ErrUnsafePath, unitPath)
			}
			contents, readErr := os.ReadFile(unitPath)
			if readErr != nil || !strings.Contains(string(contents), ManagedUnitMarker) {
				return nil, fmt.Errorf("%w: %s", ErrUnownedPath, unitPath)
			}
			names = append(names, unit.name)
		}
	}
	return names, nil
}

func Install(options Options) (Result, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return Result{}, err
	}
	if err := ensurePathDirectory(options.DataRoot, 0o700, true); err != nil {
		return Result{}, err
	}
	lock, err := storage.AcquireDataLock(options.DataRoot, ".install.lock")
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	roles := expandRoles(options.Role)
	result := Result{}
	for _, role := range roles {
		manifest, err := installRole(options, role)
		if err != nil {
			return result, err
		}
		result.Manifests = append(result.Manifests, manifest)
	}
	return result, nil
}

func Uninstall(options Options, purge bool) (Result, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return Result{}, err
	}
	if err := ensurePathDirectory(options.DataRoot, 0o700, true); err != nil {
		return Result{}, err
	}
	lock, err := storage.AcquireDataLock(options.DataRoot, ".install.lock")
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	result := Result{}
	for _, role := range expandRoles(options.Role) {
		manifest, err := loadManifest(options, role)
		if err != nil {
			return result, err
		}
		if err := validateManifest(options, role, manifest); err != nil {
			return result, err
		}
		for _, owned := range manifest.Paths {
			if !owned.Uninstall && !purge {
				result.Kept = append(result.Kept, owned.Path)
				continue
			}
			if !owned.Uninstall && !owned.Purge {
				result.Kept = append(result.Kept, owned.Path)
				continue
			}
			if owned.Kind == "directory" && !purge {
				result.Kept = append(result.Kept, owned.Path)
				continue
			}
			if err := removeOwnedPath(owned, purge); err != nil {
				return result, err
			}
			result.Removed = append(result.Removed, owned.Path)
		}
	}
	return result, nil
}

func installRole(options Options, role Role) (Manifest, error) {
	paths, err := expectedPaths(options, role)
	if err != nil {
		return Manifest{}, err
	}
	dataDir := roleDataDir(options, role)
	if err := ensurePathDirectory(options.DataRoot, 0o700, true); err != nil {
		return Manifest{}, err
	}
	if err := ensurePathDirectory(dataDir, 0o700, true); err != nil {
		return Manifest{}, err
	}
	if err := ensurePathDirectory(options.ProjectRoot, 0o755, false); err != nil {
		return Manifest{}, err
	}
	if err := ensurePathDirectory(filepath.Join(options.ProjectRoot, "bin"), 0o755, false); err != nil {
		return Manifest{}, err
	}
	if err := ensurePathDirectory(options.LogRoot, 0o750, false); err != nil {
		return Manifest{}, err
	}
	if err := ensurePathDirectory(options.SystemdDir, 0o755, false); err != nil {
		return Manifest{}, err
	}

	manifest, found, err := readExistingManifest(options, role)
	if err != nil {
		return Manifest{}, err
	}
	if found {
		if err := validateManifest(options, role, manifest); err != nil {
			return Manifest{}, err
		}
	} else if entries, readErr := os.ReadDir(dataDir); readErr != nil {
		return Manifest{}, readErr
	} else if len(entries) != 0 {
		return Manifest{}, fmt.Errorf("%w: %s", ErrUnownedPath, dataDir)
	}

	ownedSet := make(map[string]bool, len(paths))
	for _, path := range manifest.Paths {
		ownedSet[path.Path] = true
	}
	previousCore := ""
	if role == RoleAgent {
		previousCore = manifest.Core.PreviousPath
	}
	if role == RoleAgent && found && manifest.Core.Owner == CoreProject && manifest.Core.Path != RuntimeCorePath(options) {
		if _, e := os.Lstat(filepath.Join(dataDir, "core", "upgrade.json")); e == nil || !errors.Is(e, os.ErrNotExist) {
			return Manifest{}, errors.New("请先恢复未完成的 Core 升级，再重装")
		}
		previousCore = manifest.Core.Path
	}
	if previousCore != "" && options.CoreOwner == CoreProject {
		paths = append(paths, legacyCoreOwnership(options, role))
	}
	created := []string{}
	rollback := true
	defer func() {
		if rollback {
			for index := len(created) - 1; index >= 0; index-- {
				_ = removeWithoutFollowing(created[index])
			}
		}
	}()

	if role == RoleControl || role == RolePanelAgent {
		if err := installBinary(options.ControlBinary, filepath.Join(options.ProjectRoot, "bin", "control"), found && ownedSet[filepath.Join(options.ProjectRoot, "bin", "control")], &created); err != nil {
			return Manifest{}, err
		}
	}
	if role == RoleAgent || role == RolePanelAgent {
		if err := installBinary(options.AgentBinary, filepath.Join(options.ProjectRoot, "bin", "control-agent"), found && ownedSet[filepath.Join(options.ProjectRoot, "bin", "control-agent")], &created); err != nil {
			return Manifest{}, err
		}
		if options.CoreOwner == CoreProject {
			corePath := RuntimeCorePath(options)
			_, directoryErr := os.Lstat(filepath.Dir(corePath))
			if directoryErr == nil && !ownedSet[filepath.Dir(corePath)] {
				return Manifest{}, fmt.Errorf("%w: %s", ErrUnownedPath, filepath.Dir(corePath))
			} else if directoryErr != nil && !errors.Is(directoryErr, os.ErrNotExist) {
				return Manifest{}, directoryErr
			}
			if err := ensurePathDirectory(filepath.Dir(corePath), 0o700, true); err != nil {
				return Manifest{}, err
			}
			if errors.Is(directoryErr, os.ErrNotExist) {
				created = append(created, filepath.Dir(corePath))
			}
			if err := installBinary(options.XrayBinary, corePath, found && ownedSet[corePath], &created); err != nil {
				return Manifest{}, err
			}
		} else if err := validateExternalCore(options.XrayBinary, dataDir, options.ProjectRoot); err != nil {
			return Manifest{}, err
		}
	}
	logPath := filepath.Join(options.LogRoot, string(role)+".log")
	if err := ensureRegularFile(logPath, 0o600, found && ownedSet[logPath], &created); err != nil {
		return Manifest{}, err
	}
	unitPath := filepath.Join(options.SystemdDir, serviceName(role)+".service")
	unit, err := renderUnit(options, role, dataDir, logPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeManagedUnit(unitPath, []byte(unit), found && ownedSet[unitPath], &created); err != nil {
		return Manifest{}, err
	}
	cleanupServicePath := filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.service")
	cleanupService, err := renderCleanupService(options, role, dataDir, logPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeManagedUnit(cleanupServicePath, []byte(cleanupService), found && ownedSet[cleanupServicePath], &created); err != nil {
		return Manifest{}, err
	}
	cleanupTimerPath := filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.timer")
	if err := writeManagedUnit(cleanupTimerPath, []byte(renderCleanupTimer(role)), found && ownedSet[cleanupTimerPath], &created); err != nil {
		return Manifest{}, err
	}

	manifest = Manifest{
		Version: ManifestVersion, Role: role, ProjectRoot: options.ProjectRoot, DataDir: dataDir,
		LogRoot: options.LogRoot, SystemdDir: options.SystemdDir,
		Core: CoreOwnership{Owner: options.CoreOwner, Path: RuntimeCorePath(options), PreviousPath: previousCore}, Paths: paths, InstalledAt: time.Now().UTC(),
	}
	manifestPath := filepath.Join(dataDir, "ownership.json")
	if err := writeOwnedFile(manifestPath, marshalManifest(manifest), 0o600, found, &created); err != nil {
		return Manifest{}, err
	}
	rollback = false
	return manifest, nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.Role != RoleControl && options.Role != RoleAgent && options.Role != RolePanelAgent {
		return Options{}, ErrUnsupportedRole
	}
	if options.ProjectRoot == "" {
		options.ProjectRoot = DefaultProjectRoot
	}
	if options.DataRoot == "" {
		options.DataRoot = DefaultDataRoot
	}
	if options.LogRoot == "" {
		options.LogRoot = DefaultLogRoot
	}
	if options.SystemdDir == "" {
		options.SystemdDir = DefaultSystemdDir
	}
	if options.CoreOwner == "" {
		options.CoreOwner = CoreProject
	}
	if options.ServiceUser == "" {
		options.ServiceUser = DefaultServiceUser
	}
	if options.ServiceGroup == "" {
		options.ServiceGroup = DefaultServiceGroup
	}
	if options.ControlListen == "" {
		options.ControlListen = DefaultControlListen
	}
	var err error
	for name, value := range map[string]*string{"project root": &options.ProjectRoot, "data root": &options.DataRoot, "log root": &options.LogRoot, "systemd directory": &options.SystemdDir} {
		*value, err = absoluteClean(*value)
		if err != nil {
			return Options{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	for name, value := range map[string]*string{"control binary": &options.ControlBinary, "agent binary": &options.AgentBinary, "Xray binary": &options.XrayBinary} {
		if *value == "" {
			continue
		}
		*value, err = absoluteClean(*value)
		if err != nil {
			return Options{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	if options.CoreOwner != CoreProject && options.CoreOwner != CoreExternal {
		return Options{}, errors.New("Core owner must be project or external")
	}
	if !serviceNamePattern.MatchString(options.ServiceUser) || !serviceNamePattern.MatchString(options.ServiceGroup) {
		return Options{}, errors.New("service user/group name is invalid")
	}
	host, _, err := net.SplitHostPort(options.ControlListen)
	if err != nil {
		return Options{}, fmt.Errorf("control listen address is invalid: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return Options{}, errors.New("control listen address must be a loopback IP")
	}
	if sameOrWithin(options.ProjectRoot, options.DataRoot) || sameOrWithin(options.DataRoot, options.ProjectRoot) {
		return Options{}, errors.New("project root and data root must be separate")
	}
	return options, nil
}

func absoluteClean(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", ErrUnsafePath
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("路径不能包含空白字符")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if filepath.Dir(abs) == abs {
		return "", ErrUnsafePath
	}
	return abs, nil
}

func expandRoles(role Role) []Role {
	if role == RolePanelAgent {
		return []Role{RoleControl, RoleAgent}
	}
	return []Role{role}
}

func roleDataDir(options Options, role Role) string {
	return filepath.Join(options.DataRoot, string(role))
}

func serviceName(role Role) string {
	return "panel-agent-" + string(role)
}

func expectedPaths(options Options, role Role) ([]PathOwnership, error) {
	return expectedPathsForCore(options, role, RuntimeCorePath(options))
}

func RuntimeCorePath(options Options) string {
	if options.CoreOwner == CoreExternal {
		return options.XrayBinary
	}
	return filepath.Join(options.DataRoot, string(RoleAgent), "runtime", "xray")
}

func legacyCoreOwnership(options Options, role Role) PathOwnership {
	return PathOwnership{Path: filepath.Join(options.ProjectRoot, "bin", "xray"), Kind: "legacy-core-binary", Role: role, Uninstall: true, Purge: true}
}

func expectedPathsForCore(options Options, role Role, corePath string) ([]PathOwnership, error) {
	dataDir := roleDataDir(options, role)
	paths := []PathOwnership{
		{Path: dataDir, Kind: "directory", Role: role, Source: "created", Purge: true},
		{Path: filepath.Join(dataDir, "ownership.json"), Kind: "manifest", Role: role, Source: "generated", Purge: true},
		{Path: filepath.Join(options.LogRoot, string(role)+".log"), Kind: "log", Role: role, Source: "generated", Purge: true},
		{Path: filepath.Join(options.SystemdDir, serviceName(role)+".service"), Kind: "systemd-unit", Role: role, Source: "generated", Uninstall: true, Purge: true},
		{Path: filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.service"), Kind: "systemd-unit", Role: role, Source: "generated", Uninstall: true, Purge: true},
		{Path: filepath.Join(options.SystemdDir, serviceName(role)+"-cleanup.timer"), Kind: "systemd-unit", Role: role, Source: "generated", Uninstall: true, Purge: true},
	}
	if role == RoleControl || role == RolePanelAgent {
		paths = append(paths, PathOwnership{Path: filepath.Join(options.ProjectRoot, "bin", "control"), Kind: "binary", Role: role, Source: options.ControlBinary, Uninstall: true, Purge: true})
	}
	if role == RoleAgent || role == RolePanelAgent {
		paths = append(paths, PathOwnership{Path: filepath.Join(options.ProjectRoot, "bin", "control-agent"), Kind: "binary", Role: role, Source: options.AgentBinary, Uninstall: true, Purge: true})
		if options.CoreOwner == CoreProject {
			if corePath == RuntimeCorePath(options) {
				paths = append(paths, PathOwnership{Path: filepath.Dir(corePath), Kind: "directory", Role: role, Source: "created", Purge: true})
			}
			paths = append(paths, PathOwnership{Path: corePath, Kind: "core-binary", Role: role, Source: options.XrayBinary, Uninstall: true, Purge: true})
		}
	}
	return paths, nil
}

func readExistingManifest(options Options, role Role) (Manifest, bool, error) {
	path := filepath.Join(roleDataDir(options, role), "ownership.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxManifestBytes {
		return Manifest{}, false, ErrManifest
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, false, err
	}
	var manifest Manifest
	if json.Unmarshal(data, &manifest) != nil {
		return Manifest{}, false, ErrManifest
	}
	return manifest, true, nil
}

func loadManifest(options Options, role Role) (Manifest, error) {
	manifest, found, err := readExistingManifest(options, role)
	if err != nil {
		return Manifest{}, err
	}
	if !found {
		return Manifest{}, fmt.Errorf("%w: %s", ErrManifest, filepath.Join(roleDataDir(options, role), "ownership.json"))
	}
	return manifest, nil
}

func validateManifest(options Options, role Role, manifest Manifest) error {
	if manifest.Version != ManifestVersion || manifest.Role != role || manifest.ProjectRoot != options.ProjectRoot || manifest.DataDir != roleDataDir(options, role) || manifest.LogRoot != options.LogRoot || manifest.SystemdDir != options.SystemdDir {
		return ErrManifest
	}
	checkOptions := options
	checkOptions.CoreOwner = manifest.Core.Owner
	expected, err := expectedPaths(checkOptions, role)
	if err != nil {
		return err
	}
	legacy := role == RoleAgent && manifest.Core.Owner == CoreProject && manifest.Core.Path != RuntimeCorePath(checkOptions)
	if legacy {
		expected, err = expectedPathsForCore(checkOptions, role, filepath.Join(options.ProjectRoot, "bin", "xray"))
		if err != nil {
			return err
		}
	}
	if !legacy && manifest.Core.PreviousPath != "" && manifest.Core.Owner == CoreProject {
		if role != RoleAgent {
			return ErrManifest
		}
		expected = append(expected, legacyCoreOwnership(options, role))
	}
	expectedByPath := map[string]PathOwnership{}
	for _, item := range expected {
		expectedByPath[item.Path] = item
	}
	if len(manifest.Paths) != len(expectedByPath) {
		return ErrManifest
	}
	for _, item := range manifest.Paths {
		want, ok := expectedByPath[item.Path]
		if !ok || item.Kind != want.Kind || item.Role != role || item.Uninstall != want.Uninstall || item.Purge != want.Purge {
			return ErrManifest
		}
	}
	if manifest.Core.Owner != CoreProject && manifest.Core.Owner != CoreExternal {
		return ErrManifest
	}
	// Uninstall and ownership checks intentionally do not require the original
	// source path. The recorded manifest remains authoritative; install still
	// compares it when a source path is supplied so a re-install cannot drift.
	if options.XrayBinary != "" && manifest.Core.Owner != options.CoreOwner {
		return ErrManifest
	}
	if legacy && manifest.Core.PreviousPath != "" {
		return ErrManifest
	}
	if manifest.Core.Owner == CoreExternal && manifest.Core.Path == "" {
		return ErrManifest
	}
	return nil
}

func installBinary(source, target string, replaceOwned bool, created *[]string) error {
	if source == "" {
		return errors.New("binary source is required")
	}
	source, err := absoluteClean(source)
	if err != nil {
		return fmt.Errorf("binary source: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("binary source is not a regular non-empty file: %s", source)
	}
	if source == target {
		return nil
	}
	if err := installFile(target, 0o755, replaceOwned, func(file *os.File) error {
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		_, err = io.Copy(file, input)
		return err
	}, created); err != nil {
		return err
	}
	return nil
}

func installFile(path string, mode os.FileMode, replaceOwned bool, write func(*os.File) error, created *[]string) error {
	if err := ensureParentPath(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !replaceOwned {
			return fmt.Errorf("%w: %s", ErrUnownedPath, path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".panel-agent-install-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := write(temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" && replaceOwned {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	if !replaceOwned {
		*created = append(*created, path)
	}
	return nil
}

func writeOwnedFile(path string, data []byte, mode os.FileMode, replaceOwned bool, created *[]string) error {
	return installFile(path, mode, replaceOwned, func(file *os.File) error {
		_, err := file.Write(data)
		return err
	}, created)
}

func writeManagedUnit(path string, data []byte, replaceOwned bool, created *[]string) error {
	if replaceOwned {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("%w: %s", ErrUnsafePath, path)
			}
			contents, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(contents), ManagedUnitMarker) {
				return fmt.Errorf("%w: %s", ErrUnownedPath, path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return writeOwnedFile(path, data, 0o644, replaceOwned, created)
}

func ensureRegularFile(path string, mode os.FileMode, replaceOwned bool, created *[]string) error {
	if err := ensureParentPath(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrUnsafePath, path)
		}
		if !replaceOwned {
			return fmt.Errorf("%w: %s", ErrUnownedPath, path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	*created = append(*created, path)
	return nil
}

func validateExternalCore(path, dataDir, projectRoot string) error {
	abs, err := absoluteClean(path)
	if err != nil {
		return fmt.Errorf("external Core: %w", err)
	}
	if sameOrWithin(abs, dataDir) || sameOrWithin(abs, projectRoot) {
		return errors.New("external Core must not be inside project-owned roots")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("external Core: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, resolveErr := filepath.EvalSymlinks(abs)
		if resolveErr != nil {
			return fmt.Errorf("external Core link: %w", resolveErr)
		}
		info, err = os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("external Core link target is not a regular file")
		}
	} else if !info.Mode().IsRegular() {
		return errors.New("external Core is not a regular file")
	}
	return nil
}

func renderUnit(options Options, role Role, dataDir, logPath string) (string, error) {
	var executable string
	var args []string
	switch role {
	case RoleControl:
		executable = filepath.Join(options.ProjectRoot, "bin", "control")
		args = []string{"serve", "--data-dir", dataDir, "--listen", options.ControlListen}
	case RoleAgent:
		executable = filepath.Join(options.ProjectRoot, "bin", "control-agent")
		args = []string{"run", "--data-dir", dataDir, "--xray-path", RuntimeCorePath(options)}
	default:
		return "", ErrUnsupportedRole
	}
	command := systemdQuote(executable)
	for _, arg := range args {
		command += " " + systemdQuote(arg)
	}
	return fmt.Sprintf(`%s
[Unit]
Description=Panel-Agent %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=5s
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ProtectKernelTunables=true
ProtectControlGroups=true
ReadWritePaths=%s %s
StandardOutput=append:%s
StandardError=append:%s
LogRateLimitIntervalSec=30s
LogRateLimitBurst=200

[Install]
WantedBy=multi-user.target
`, ManagedUnitMarker, role, options.ServiceUser, options.ServiceGroup, dataDir, command, dataDir, options.LogRoot, logPath, logPath), nil
}

func renderCleanupService(options Options, role Role, dataDir, logPath string) (string, error) {
	var executable string
	switch role {
	case RoleControl:
		executable = filepath.Join(options.ProjectRoot, "bin", "control")
	case RoleAgent:
		executable = filepath.Join(options.ProjectRoot, "bin", "control-agent")
	default:
		return "", ErrUnsupportedRole
	}
	command := systemdQuote(executable) + " cleanup --data-dir " + systemdQuote(dataDir) + " --log-root " + systemdQuote(options.LogRoot)
	return fmt.Sprintf(`%s
[Unit]
Description=Panel-Agent %s cleanup
After=%s.service

[Service]
Type=oneshot
User=%s
Group=%s
ExecStart=%s
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=%s %s
StandardOutput=append:%s
StandardError=append:%s
`, ManagedUnitMarker, role, serviceName(role), options.ServiceUser, options.ServiceGroup, command, dataDir, options.LogRoot, logPath, logPath), nil
}

func renderCleanupTimer(role Role) string {
	return fmt.Sprintf(`%s
[Unit]
Description=Panel-Agent %s cleanup timer

[Timer]
OnBootSec=15min
OnUnitActiveSec=15min
Persistent=true
Unit=%s-cleanup.service

[Install]
WantedBy=timers.target
`, ManagedUnitMarker, role, serviceName(role))
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `$`, `\$`)
	value = strings.ReplaceAll(value, `%`, `%%`)
	return `"` + value + `"`
}

func ensureParentPath(path string) error {
	return ensurePathNoSymlink(filepath.Dir(path))
}

func ensurePathDirectory(path string, mode os.FileMode, private bool) error {
	_, existedErr := os.Lstat(path)
	existed := existedErr == nil
	if existedErr != nil && !errors.Is(existedErr, os.ErrNotExist) {
		return existedErr
	}
	if err := ensurePathNoSymlink(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrUnsafePath, path)
	}
	if !existed {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
	}
	if private && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private directory permissions are too broad: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("directory is writable by other users: %s", path)
	}
	return nil
}

func ensurePathNoSymlink(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	for len(rest) > 0 && (rest[0] == filepath.Separator || rest[0] == '/') {
		rest = rest[1:]
	}
	current := volume + string(filepath.Separator)
	if volume == "" && !filepath.IsAbs(abs) {
		return ErrUnsafePath
	}
	if volume != "" && runtime.GOOS == "windows" {
		current = volume + string(filepath.Separator)
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s", ErrUnsafePath, current)
		}
	}
	return nil
}

func sameOrWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func removeOwnedPath(item PathOwnership, purge bool) error {
	if item.Kind == "directory" && !purge {
		return nil
	}
	info, err := os.Lstat(item.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(item.Path)
	}
	if item.Kind == "directory" {
		return removeTree(item.Path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrUnsafePath, item.Path)
	}
	return os.Remove(item.Path)
}

func removeTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeTree(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func removeWithoutFollowing(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return removeTree(path)
	}
	return os.Remove(path)
}

func marshalManifest(manifest Manifest) []byte {
	data, _ := json.MarshalIndent(manifest, "", "  ")
	return data
}
