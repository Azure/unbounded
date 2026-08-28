// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bufio"
	"context"
	"fmt"
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
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkIsPrivilegedUserName    = "is-privileged-user"
	checkHostPackagesName        = "host-packages"
	checkHostOSConfigurationName = "host-os-configuration"
	checkNSpawnRuntimeName       = "nspawn-runtime"
	checkDockerActiveName        = "docker-active"
	checkContainerdActiveName    = "containerd-active"
	checkKubeletActiveName       = "kubelet-active"
	checkSwapActiveName          = "swap-active"
	checkDiskSpaceName           = "disk-space"
	checkCgroupsName             = "cgroups"

	minFreeDiskBytes = 8 * 1024 * 1024 * 1024
)

type hostCheckDeps struct {
	lookupPath           func(string) (string, error)
	detectPackageManager func(func(string) (string, error)) (*hostPackageManager, error)
	uid                  func() int
	statfs               func(string, *syscall.Statfs_t) error
	readFile             func(string) ([]byte, error)
	stat                 func(string) (fs.FileInfo, error)
	writeProbe           func(string) error
	outputCmd            func(context.Context, *slog.Logger, string, ...string) (string, error)
}

func defaultHostCheckDeps() hostCheckDeps {
	return hostCheckDeps{
		lookupPath:           exec.LookPath,
		detectPackageManager: detectHostPackageManager,
		uid:                  os.Geteuid,
		statfs:               syscall.Statfs,
		readFile:             os.ReadFile,
		stat:                 os.Stat,
		writeProbe:           utilio.ProbeWritableDir,
		outputCmd:            executil.OutputCmd,
	}
}

type simpleHostChecker struct {
	name  string
	check func(context.Context) []preflight.Result
}

func (c simpleHostChecker) Name() string { return c.name }

func (c simpleHostChecker) Check(ctx context.Context) []preflight.Result { return c.check(ctx) }

// Preflight returns the standard host environment checks required before
// provisioning an nspawn machine.
func Preflight(log *slog.Logger, cfg config.AgentConfig, _ *goalstates.MachineGoalState) []preflight.Checker {
	checks := []preflight.Checker{
		CheckIsPrivilegedUser(log),
		CheckExistingDeployment(log, cfg.HostPrefix),
		checkHostPackages(log, cfg.OfflineArtifactsConfigured(), defaultHostCheckDeps()),
		CheckHostOSConfiguration(log, cfg),
		CheckNSpawnRuntime(log, cfg),
		CheckDockerActive(log),
		CheckContainerdActive(log),
		CheckKubeletActive(log),
		CheckSwapActive(log),
		CheckDiskSpace(log),
		CheckCgroups(log),
		CheckNvidiaDriver(log),
		checkLocalDNSConntrack(log, cfg, defaultLocalDNSConntrackDeps()),
	}

	return checks
}

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
	return checkHostPackages(log, false, defaultHostCheckDeps())
}

func checkHostPackages(log *slog.Logger, failMissing bool, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkHostPackagesName, check: func(ctx context.Context) []preflight.Result {
		pm, err := deps.detectPackageManager(deps.lookupPath)
		if err != nil {
			log.Debug("host package manager detection failed")

			// Report the detection error itself. On a host with no package
			// manager it names the specific tools that are missing, which is
			// actionable; the older message claimed a package manager was
			// required, which is not true of immutable hosts that ship the
			// tools directly.
			return preflight.ResultsError(
				checkHostPackagesName,
				"host packages",
				"host packages cannot be verified: %s",
				err.Error(),
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
				missing = append(missing, pkg)
			}
		}

		if len(missing) > 0 {
			log.Debug("required host packages are missing", "packages", strings.Join(missing, ","))

			if failMissing {
				return preflight.ResultsError(
					checkHostPackagesName,
					"host packages",
					"required host packages are missing and cannot be installed automatically when OfflineArtifacts is configured: %s",
					strings.Join(missing, ", "),
				)
			}

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
func CheckHostOSConfiguration(log *slog.Logger, cfg config.AgentConfig) preflight.Checker {
	return checkHostOSConfiguration(log, cfg, defaultHostCheckDeps())
}

func checkHostOSConfiguration(log *slog.Logger, cfg config.AgentConfig, deps hostCheckDeps) preflight.Checker {
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

		results = append(results, hostPrefixResults(log, cfg, deps)...)

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

// hostPrefixResults verifies the agent can write its own host-side files under
// the configured installation prefix.
//
// The prefix is never inferred. A host whose default prefix is not writable,
// such as one with a read-only /usr, must declare a writable HostPrefix; the
// agent refuses rather than silently relocating its files, because a wrong
// guess would place binaries that generated systemd units already reference by
// absolute path.
func hostPrefixResults(log *slog.Logger, cfg config.AgentConfig, deps hostCheckDeps) []preflight.Result {
	var results []preflight.Result

	hostPaths := goalstates.ResolveHostPaths(cfg.HostPrefix)

	dirs := []string{hostPaths.BinDir}
	if cfg.LocalDNS != nil && cfg.LocalDNS.Enabled {
		dirs = append(dirs, hostPaths.LibexecDir)
	}

	for _, dir := range dirs {
		log.Debug("checking host install directory", "path", dir)

		if err := deps.writeProbe(dir); err == nil {
			continue
		}

		if strings.TrimSpace(cfg.HostPrefix) == "" {
			results = append(results, preflight.Error(
				checkHostOSConfigurationName,
				"host OS configuration",
				"agent install directory is not writable: %s; set HostPrefix to a writable prefix, "+
					"which is required on hosts with a read-only /usr",
				dir,
			))

			continue
		}

		results = append(results, preflight.Error(
			checkHostOSConfigurationName,
			"host OS configuration",
			"configured HostPrefix is not writable: %s",
			dir,
		))
	}

	return results
}

// CheckNSpawnRuntime verifies systemd-nspawn runtime tools are available.
func CheckNSpawnRuntime(log *slog.Logger, cfg config.AgentConfig) preflight.Checker {
	return checkNSpawnRuntime(log, cfg, defaultHostCheckDeps())
}

func checkNSpawnRuntime(log *slog.Logger, cfg config.AgentConfig, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkNSpawnRuntimeName, check: func(context.Context) []preflight.Result {
		var results []preflight.Result

		// A missing tool is only a warning when bootstrap can still supply it,
		// either through a package manager or a configured system extension.
		// When neither can, nothing downstream will fix it, so report an error
		// here rather than failing later with a less obvious message.
		remediable := cfg.SystemExtensionConfigured()
		if !remediable {
			if _, err := deps.detectPackageManager(deps.lookupPath); err == nil {
				remediable = true
			}
		}

		for _, binary := range []string{"systemctl", "machinectl", "systemd-nspawn"} {
			log.Debug("checking nspawn runtime tool", "binary", binary)

			if _, err := deps.lookupPath(binary); err != nil {
				if !remediable {
					results = append(results, preflight.Error(
						checkNSpawnRuntimeName,
						"nspawn runtime",
						"nspawn runtime tool is missing and cannot be installed on this host: %s; "+
							"configure SystemExtension to supply it",
						binary,
					))

					continue
				}

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
	return checkSystemdUnitActive(log, deps, checkDockerActiveName, dockerServiceUnit, "Docker")
}

// CheckContainerdActive warns when the host containerd service is active.
func CheckContainerdActive(log *slog.Logger) preflight.Checker {
	return checkContainerdActive(log, defaultHostCheckDeps())
}

func checkContainerdActive(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return checkSystemdUnitActive(log, deps, checkContainerdActiveName, containerdServiceUnit, "containerd")
}

// CheckKubeletActive warns when the host kubelet service is active.
func CheckKubeletActive(log *slog.Logger) preflight.Checker {
	return checkKubeletActive(log, defaultHostCheckDeps())
}

func checkKubeletActive(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return checkSystemdUnitActive(log, deps, checkKubeletActiveName, kubeletServiceUnit, "kubelet")
}

func checkSystemdUnitActive(
	log *slog.Logger,
	deps hostCheckDeps,
	checkName string,
	unit string,
	serviceName string,
) preflight.Checker {
	return simpleHostChecker{name: checkName, check: func(ctx context.Context) []preflight.Result {
		out, err := deps.outputCmd(ctx, log, "systemctl", "is-active", unit)
		log.Debug(
			"checked host systemd unit state",
			"unit", unit,
			"state", strings.TrimSpace(out),
			"error", err != nil,
		)

		if err != nil {
			return preflight.ResultsWarning(
				checkName,
				unit,
				"%s state could not be determined",
				serviceName,
			)
		}

		if strings.TrimSpace(out) == "active" {
			return preflight.ResultsWarning(
				checkName,
				unit,
				"%s is active and bootstrap will disable it",
				serviceName,
			)
		}

		return preflight.ResultsOK(checkName, unit, fmt.Sprintf("%s is not active", serviceName))
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
