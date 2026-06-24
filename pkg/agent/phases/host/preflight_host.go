// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bufio"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	CheckIsPrivilegedUserName    = "is-privileged-user"
	CheckHostPackagesName        = "host-packages"
	CheckHostPackageSourcesName  = "host-package-sources"
	CheckHostOSConfigurationName = "host-os-configuration"
	CheckNSpawnRuntimeName       = "nspawn-runtime"
	CheckDockerActiveName        = "docker-active"
	CheckSwapActiveName          = "swap-active"
	CheckDiskSpaceName           = "disk-space"
	CheckCgroupsName             = "cgroups"
	CheckNodeIdentityName        = "node-identity"

	minFreeDiskBytes = 8 * 1024 * 1024 * 1024
)

type hostCheckDeps struct {
	lookupPath func(string) (string, error)
	uid        func() int
	statfs     func(string, *syscall.Statfs_t) error
	readFile   func(string) ([]byte, error)
	stat       func(string) (fs.FileInfo, error)
	writeProbe func(string) error
	outputCmd  func(context.Context, *slog.Logger, string, ...string) (string, error)
}

func defaultHostCheckDeps() hostCheckDeps {
	return hostCheckDeps{
		lookupPath: exec.LookPath,
		uid:        os.Geteuid,
		statfs:     syscall.Statfs,
		readFile:   os.ReadFile,
		stat:       os.Stat,
		writeProbe: probeWritableDir,
		outputCmd:  executil.OutputCmd,
	}
}

type simpleHostChecker struct {
	name  string
	check func(context.Context) []preflight.Result
}

func (c simpleHostChecker) Name() string { return c.name }

func (c simpleHostChecker) Check(ctx context.Context) []preflight.Result { return c.check(ctx) }

func CheckIsPrivilegedUser() preflight.Checker {
	return checkIsPrivilegedUser(defaultHostCheckDeps())
}

func checkIsPrivilegedUser(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckIsPrivilegedUserName, check: func(context.Context) []preflight.Result {
		if deps.uid() != 0 {
			return preflight.ResultsError(CheckIsPrivilegedUserName, "host user", "preflight must run as root")
		}

		return preflight.ResultsOK(CheckIsPrivilegedUserName, "host user", "preflight is running as root")
	}}
}

func CheckHostPackages(log *slog.Logger) preflight.Checker {
	return checkHostPackages(log, defaultHostCheckDeps())
}

func checkHostPackages(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckHostPackagesName, check: func(ctx context.Context) []preflight.Result {
		pm, err := detectHostPackageManager(deps.lookupPath)
		if err != nil {
			return preflight.ResultsError(CheckHostPackagesName, "host packages", "supported host package manager is required")
		}

		var missing []string

		for _, pkg := range pm.requiredPackages {
			if !pm.installed(ctx, log, pkg) {
				missing = append(missing, pkg)
			}
		}

		if len(missing) > 0 {
			return preflight.ResultsError(CheckHostPackagesName, "host packages", "required host packages are missing")
		}

		return preflight.ResultsOK(CheckHostPackagesName, "host packages", "required host packages are installed")
	}}
}

func CheckHostPackageSources(log *slog.Logger) preflight.Checker {
	return checkHostPackageSources(log, defaultHostCheckDeps())
}

func checkHostPackageSources(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckHostPackageSourcesName, check: func(ctx context.Context) []preflight.Result {
		pm, err := detectHostPackageManager(deps.lookupPath)
		if err != nil {
			return preflight.ResultsError(CheckHostPackageSourcesName, "host package sources", "supported host package manager is required")
		}

		for _, pkg := range pm.requiredPackages {
			if !pm.installed(ctx, log, pkg) {
				return preflight.ResultsWarning(CheckHostPackageSourcesName, "host package sources", "package sources may be required for missing host packages")
			}
		}

		return preflight.ResultsOK(CheckHostPackageSourcesName, "host package sources", "package source access is not required")
	}}
}

func CheckHostOSConfiguration() preflight.Checker {
	return checkHostOSConfiguration(defaultHostCheckDeps())
}

func checkHostOSConfiguration(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckHostOSConfigurationName, check: func(context.Context) []preflight.Result {
		if err := deps.writeProbe(filepath.Dir(hostSysctlPath)); err != nil {
			return preflight.ResultsError(CheckHostOSConfigurationName, "host OS configuration", "host OS configuration paths are not writable")
		}

		if err := deps.writeProbe(goalstates.SystemdSystemDir); err != nil {
			return preflight.ResultsError(CheckHostOSConfigurationName, "host OS configuration", "systemd unit directory is not writable")
		}

		return preflight.ResultsOK(CheckHostOSConfigurationName, "host OS configuration", "host OS configuration can be applied")
	}}
}

func CheckNSpawnRuntime() preflight.Checker {
	return checkNSpawnRuntime(defaultHostCheckDeps())
}

func checkNSpawnRuntime(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckNSpawnRuntimeName, check: func(context.Context) []preflight.Result {
		for _, binary := range []string{"systemctl", "machinectl", "systemd-nspawn"} {
			if _, err := deps.lookupPath(binary); err != nil {
				return preflight.ResultsError(CheckNSpawnRuntimeName, "nspawn runtime", "nspawn runtime tools are required")
			}
		}

		if _, err := deps.stat("/run/systemd/system"); err != nil {
			return preflight.ResultsError(CheckNSpawnRuntimeName, "nspawn runtime", "systemd runtime is required")
		}

		return preflight.ResultsOK(CheckNSpawnRuntimeName, "nspawn runtime", "nspawn runtime is available")
	}}
}

func CheckDockerActive(log *slog.Logger) preflight.Checker {
	return checkDockerActive(log, defaultHostCheckDeps())
}

func checkDockerActive(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckDockerActiveName, check: func(ctx context.Context) []preflight.Result {
		out, err := deps.outputCmd(ctx, log, "systemctl", "is-active", dockerServiceUnit)
		if err == nil && strings.TrimSpace(out) == "active" {
			return preflight.ResultsWarning(CheckDockerActiveName, "docker service", "Docker is active and bootstrap will disable it")
		}

		return preflight.ResultsOK(CheckDockerActiveName, "docker service", "Docker is not active")
	}}
}

func CheckSwapActive() preflight.Checker {
	return checkSwapActive(defaultHostCheckDeps())
}

func checkSwapActive(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckSwapActiveName, check: func(context.Context) []preflight.Result {
		active, err := swapActive(deps.readFile)
		if err != nil {
			return preflight.ResultsWarning(CheckSwapActiveName, "host swap", "swap state could not be determined")
		}

		if active {
			return preflight.ResultsWarning(CheckSwapActiveName, "host swap", "swap is enabled and bootstrap will disable it")
		}

		return preflight.ResultsOK(CheckSwapActiveName, "host swap", "swap is not active")
	}}
}

func CheckDiskSpace() preflight.Checker {
	return checkDiskSpace(defaultHostCheckDeps())
}

func checkDiskSpace(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckDiskSpaceName, check: func(context.Context) []preflight.Result {
		var stat syscall.Statfs_t
		if err := deps.statfs("/var/lib", &stat); err != nil {
			return preflight.ResultsError(CheckDiskSpaceName, "host disk", "available disk space could not be determined")
		}

		free := stat.Bavail * uint64(stat.Bsize)
		if free < minFreeDiskBytes {
			return preflight.ResultsError(CheckDiskSpaceName, "host disk", "available disk space is below the minimum")
		}

		return preflight.ResultsOK(CheckDiskSpaceName, "host disk", "sufficient disk space is available")
	}}
}

func CheckCgroups() preflight.Checker {
	return checkCgroups(defaultHostCheckDeps())
}

func checkCgroups(deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: CheckCgroupsName, check: func(context.Context) []preflight.Result {
		if _, err := deps.stat("/sys/fs/cgroup"); err != nil {
			return preflight.ResultsError(CheckCgroupsName, "host cgroups", "cgroup filesystem is required")
		}

		return preflight.ResultsOK(CheckCgroupsName, "host cgroups", "cgroup filesystem is available")
	}}
}

func CheckNodeIdentity(cfg *config.AgentConfig) preflight.Checker {
	return simpleHostChecker{name: CheckNodeIdentityName, check: func(context.Context) []preflight.Result {
		if cfg == nil || strings.TrimSpace(cfg.NodeName) == "" {
			return preflight.ResultsError(CheckNodeIdentityName, "node identity", "node name could not be resolved")
		}

		return preflight.ResultsOK(CheckNodeIdentityName, "node identity", "node name is resolved")
	}}
}

func probeWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".unbounded-preflight-*")
	if err != nil {
		return err
	}

	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name) //nolint:errcheck // best effort cleanup after close failure.
		return err
	}

	return os.Remove(name)
}

func swapActive(readFile func(string) ([]byte, error)) (bool, error) {
	data, err := readFile("/proc/swaps")
	if err != nil {
		return false, err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "Filename") {
			continue
		}

		if strings.TrimSpace(scanner.Text()) != "" {
			return true, nil
		}
	}

	return false, scanner.Err()
}
