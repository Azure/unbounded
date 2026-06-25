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
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkIsPrivilegedUserName    = "is-privileged-user"
	checkHostPackagesName        = "host-packages"
	checkHostOSConfigurationName = "host-os-configuration"
	checkNSpawnRuntimeName       = "nspawn-runtime"
	checkDockerActiveName        = "docker-active"
	checkSwapActiveName          = "swap-active"
	checkDiskSpaceName           = "disk-space"
	checkCgroupsName             = "cgroups"

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
		writeProbe: utilio.ProbeWritableDir,
		outputCmd:  executil.OutputCmd,
	}
}

type simpleHostChecker struct {
	name  string
	check func(context.Context) []preflight.Result
}

func (c simpleHostChecker) Name() string { return c.name }

func (c simpleHostChecker) Check(ctx context.Context) []preflight.Result { return c.check(ctx) }

// CheckIsPrivilegedUser verifies preflight is running as root.
func CheckIsPrivilegedUser(log *slog.Logger) preflight.Checker {
	return checkIsPrivilegedUser(log, defaultHostCheckDeps())
}

func checkIsPrivilegedUser(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkIsPrivilegedUserName, check: func(context.Context) []preflight.Result {
		uid := deps.uid()
		log.Debug("checking effective user", "uid", uid)

		if uid != 0 {
			return preflight.ResultsError(checkIsPrivilegedUserName, "host user", "preflight must run as root")
		}

		return preflight.ResultsOK(checkIsPrivilegedUserName, "host user", "preflight is running as root")
	}}
}

// CheckHostPackages verifies all required host packages are already installed.
func CheckHostPackages(log *slog.Logger) preflight.Checker {
	return checkHostPackages(log, defaultHostCheckDeps())
}

func checkHostPackages(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkHostPackagesName, check: func(ctx context.Context) []preflight.Result {
		pm, err := detectHostPackageManager(deps.lookupPath)
		if err != nil {
			log.Debug("host package manager detection failed")

			return preflight.ResultsError(
				checkHostPackagesName,
				"host packages",
				"supported host package manager is required: apt-get, tdnf, or dnf",
			)
		}

		log.Debug(
			"detected host package manager",
			"packageManager", pm.name,
			"requiredPackages", strings.Join(pm.requiredPackages, ","),
		)

		var missing []string

		for _, pkg := range pm.requiredPackages {
			if !pm.installed(ctx, log, pkg) {
				// TODO: when offline mode is configured, missing required host
				// packages should be reported as an error because bootstrap cannot
				// rely on package source access to remediate them.
				missing = append(missing, pkg)
			}
		}

		if len(missing) > 0 {
			log.Debug("required host packages are missing", "packages", strings.Join(missing, ","))

			// TODO: when offline mode is configured, missing required host
			// packages should be reported as an error because bootstrap cannot
			// rely on package source access to remediate them.
			return preflight.ResultsWarning(
				checkHostPackagesName,
				"host packages",
				"required host packages are missing and may be installed by bootstrap: %s",
				strings.Join(missing, ", "),
			)
		}

		log.Debug("required host packages are installed")

		return preflight.ResultsOK(checkHostPackagesName, "host packages", "required host packages are installed")
	}}
}

// CheckHostOSConfiguration verifies host OS configuration paths are writable.
func CheckHostOSConfiguration(log *slog.Logger) preflight.Checker {
	return checkHostOSConfiguration(log, defaultHostCheckDeps())
}

func checkHostOSConfiguration(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkHostOSConfigurationName, check: func(context.Context) []preflight.Result {
		var results []preflight.Result

		sysctlDir := filepath.Dir(hostSysctlPath)
		log.Debug("checking host OS configuration path", "path", sysctlDir)

		if err := deps.writeProbe(sysctlDir); err != nil {
			results = append(results, preflight.Error(
				checkHostOSConfigurationName,
				"host OS configuration",
				"host OS configuration path is not writable: %s",
				sysctlDir,
			))
		}

		log.Debug("checking systemd unit directory", "path", goalstates.SystemdSystemDir)

		if err := deps.writeProbe(goalstates.SystemdSystemDir); err != nil {
			results = append(results, preflight.Error(
				checkHostOSConfigurationName,
				"host OS configuration",
				"systemd unit directory is not writable: %s",
				goalstates.SystemdSystemDir,
			))
		}

		if len(results) > 0 {
			return results
		}

		return preflight.ResultsOK(
			checkHostOSConfigurationName,
			"host OS configuration",
			"host OS configuration can be applied",
		)
	}}
}

// CheckNSpawnRuntime verifies systemd-nspawn runtime tools are available.
func CheckNSpawnRuntime(log *slog.Logger) preflight.Checker {
	return checkNSpawnRuntime(log, defaultHostCheckDeps())
}

func checkNSpawnRuntime(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkNSpawnRuntimeName, check: func(context.Context) []preflight.Result {
		var results []preflight.Result

		for _, binary := range []string{"systemctl", "machinectl", "systemd-nspawn"} {
			log.Debug("checking nspawn runtime tool", "binary", binary)

			if _, err := deps.lookupPath(binary); err != nil {
				// TODO: when offline mode is configured, missing nspawn runtime
				// tools should be reported as an error because bootstrap cannot rely
				// on package installation to remediate them.
				results = append(results, preflight.Warning(
					checkNSpawnRuntimeName,
					"nspawn runtime",
					"nspawn runtime tool is missing and may be installed by bootstrap: %s",
					binary,
				))
			}
		}

		systemdRuntimePath := "/run/systemd/system"
		log.Debug("checking systemd runtime path", "path", systemdRuntimePath)

		if _, err := deps.stat(systemdRuntimePath); err != nil {
			results = append(results, preflight.Warning(
				checkNSpawnRuntimeName,
				"nspawn runtime",
				"systemd runtime path is not currently available: %s",
				systemdRuntimePath,
			))
		}

		if len(results) > 0 {
			return results
		}

		return preflight.ResultsOK(checkNSpawnRuntimeName, "nspawn runtime", "nspawn runtime is available")
	}}
}

// CheckDockerActive warns when Docker is active.
func CheckDockerActive(log *slog.Logger) preflight.Checker {
	return checkDockerActive(log, defaultHostCheckDeps())
}

func checkDockerActive(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkDockerActiveName, check: func(ctx context.Context) []preflight.Result {
		out, err := deps.outputCmd(ctx, log, "systemctl", "is-active", dockerServiceUnit)
		log.Debug(
			"checked Docker unit state",
			"unit", dockerServiceUnit,
			"state", strings.TrimSpace(out),
			"error", err != nil,
		)

		if err == nil && strings.TrimSpace(out) == "active" {
			return preflight.ResultsWarning(
				checkDockerActiveName,
				"docker service",
				"Docker is active and bootstrap will disable it",
			)
		}

		return preflight.ResultsOK(checkDockerActiveName, "docker service", "Docker is not active")
	}}
}

// CheckSwapActive warns when host swap is active.
func CheckSwapActive(log *slog.Logger) preflight.Checker {
	return checkSwapActive(log, defaultHostCheckDeps())
}

func checkSwapActive(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkSwapActiveName, check: func(context.Context) []preflight.Result {
		active, err := swapActive(deps.readFile)
		log.Debug("checked host swap state", "active", active, "error", err != nil)

		if err != nil {
			return preflight.ResultsWarning(
				checkSwapActiveName,
				"host swap",
				"swap state could not be determined from /proc/swaps",
			)
		}

		if active {
			return preflight.ResultsWarning(checkSwapActiveName, "host swap", "swap is enabled and bootstrap will disable it")
		}

		return preflight.ResultsOK(checkSwapActiveName, "host swap", "swap is not active")
	}}
}

// CheckDiskSpace verifies enough free disk is available for bootstrap.
func CheckDiskSpace(log *slog.Logger) preflight.Checker {
	return checkDiskSpace(log, defaultHostCheckDeps())
}

func checkDiskSpace(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkDiskSpaceName, check: func(context.Context) []preflight.Result {
		var stat syscall.Statfs_t

		diskPath := "/var/lib"
		if err := deps.statfs(diskPath, &stat); err != nil {
			log.Debug("failed to check disk space", "path", diskPath)

			return preflight.ResultsError(
				checkDiskSpaceName,
				"host disk",
				"available disk space could not be determined for %s",
				diskPath,
			)
		}

		free := stat.Bavail * uint64(stat.Bsize)
		log.Debug(
			"checked disk space",
			"path", diskPath,
			"freeGiB", gib(free),
			"requiredGiB", gib(minFreeDiskBytes),
		)

		if free < minFreeDiskBytes {
			return preflight.ResultsError(
				checkDiskSpaceName,
				"host disk",
				"available disk space is below the minimum for %s: current %.1f GiB, required %.1f GiB",
				diskPath,
				gib(free),
				gib(minFreeDiskBytes),
			)
		}

		return preflight.ResultsOK(checkDiskSpaceName, "host disk", "sufficient disk space is available")
	}}
}

// CheckCgroups verifies the host cgroup filesystem is available.
func CheckCgroups(log *slog.Logger) preflight.Checker {
	return checkCgroups(log, defaultHostCheckDeps())
}

func checkCgroups(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkCgroupsName, check: func(context.Context) []preflight.Result {
		cgroupPath := "/sys/fs/cgroup"
		log.Debug("checking cgroup filesystem", "path", cgroupPath)

		if _, err := deps.stat(cgroupPath); err != nil {
			return preflight.ResultsError(checkCgroupsName, "host cgroups", "cgroup filesystem is required at %s", cgroupPath)
		}

		return preflight.ResultsOK(checkCgroupsName, "host cgroups", "cgroup filesystem is available")
	}}
}

func gib(bytes uint64) float64 {
	return float64(bytes) / (1024 * 1024 * 1024)
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
