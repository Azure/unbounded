// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckIsPrivilegedUser(t *testing.T) {
	results := checkIsPrivilegedUser(slog.New(slog.DiscardHandler), hostCheckDeps{uid: func() int { return 0 }}).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	results = checkIsPrivilegedUser(slog.New(slog.DiscardHandler), hostCheckDeps{uid: func() int { return 1000 }}).Check(context.Background())
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
}

func TestCheckHostPackagesMissingPackageManager(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.lookupPath = lookupPathWith(nil)

	results := checkHostPackages(slog.New(slog.DiscardHandler), false, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "apt-get")
}

func TestCheckHostPackagesListsMissingPackages(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.detectPackageManager = packageManagerWithInstalled(false)

	results := checkHostPackages(slog.New(slog.DiscardHandler), false, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Contains(t, results[0].Message, "systemd-container")
}

func TestCheckHostPackagesBlocksMissingPackagesWhenOfflineArtifactsConfigured(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.detectPackageManager = packageManagerWithInstalled(false)

	results := checkHostPackages(slog.New(slog.DiscardHandler), true, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "OfflineArtifacts")
	assert.Contains(t, results[0].Message, "systemd-container")
}

func TestCheckHostOSConfiguration(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.writeProbe = func(string) error { return nil }

	results := checkHostOSConfiguration(slog.New(slog.DiscardHandler), deps, "").Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.writeProbe = func(string) error { return errors.New("denied") }
	results = checkHostOSConfiguration(slog.New(slog.DiscardHandler), deps, "").Check(context.Background())
	assert.Len(t, results, 3)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "/etc/sysctl.d")
	assert.Equal(t, preflight.SeverityError, results[1].Severity)
	assert.Contains(t, results[1].Message, "systemd")
	assert.Equal(t, preflight.SeverityError, results[2].Severity)
	assert.Contains(t, results[2].Message, "agent install directory")
}

// TestAgentInstallDirsProbeIsCreatable covers the normal state of a host that
// has never been bootstrapped: the agent's install directory does not exist
// yet. The question is whether it can be created, so an absent directory whose
// parent is writable must pass, and only a genuinely unwritable location fails.
func TestAgentInstallDirsProbeIsCreatable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "bin")

	var probed []string

	deps := defaultHostCheckDeps()
	deps.writeProbe = func(dir string) error {
		probed = append(probed, dir)
		return nil
	}

	results := installDirResults(slog.New(slog.DiscardHandler), []string{target}, deps)
	assert.Empty(t, results, "an absent directory under a writable parent is fine")
	assert.Equal(t, []string{root}, probed, "the nearest existing ancestor is probed, not the absent directory")

	deps.writeProbe = func(string) error { return errors.New("read-only file system") }
	results = installDirResults(slog.New(slog.DiscardHandler), []string{target}, deps)
	assert.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, target)
	assert.Contains(t, results[0].Message, root)
}

// TestAgentInstallDirsFollowTheInstallationPrefix keeps the checked directory
// tied to where the agent actually installs, so the two cannot drift apart.
//
// The prefix case is the one that matters. Preflight runs before anything is
// written, and it refuses rather than warns, so checking a fixed /usr/local on
// a host that configured a prefix reports a host that cannot be provisioned
// when it can. On an immutable host that default is read-only, which means
// bootstrap never starts at all and the reason given is a directory the agent
// was never going to use.
func TestAgentInstallDirsFollowTheInstallationPrefix(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prefix string
		want   string
	}{
		"unset prefix keeps the historical directory": {
			prefix: "",
			want:   filepath.Dir(goalstates.DaemonBinaryPath),
		},
		"configured prefix moves it": {
			prefix: "/opt/unbounded",
			want:   "/opt/unbounded/bin",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dirs := agentInstallDirs(tc.prefix)
			assert.Len(t, dirs, 1)
			assert.Equal(t, tc.want, dirs[0])
		})
	}
}

// TestCheckHostOSConfigurationProbesThePrefix is the end-to-end form: the check
// must not fail a host whose prefix is writable merely because the default is
// not. This is the failure that stopped an immutable host from bootstrapping.
func TestCheckHostOSConfigurationProbesThePrefix(t *testing.T) {
	t.Parallel()

	deps := defaultHostCheckDeps()
	deps.stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	deps.writeProbe = func(dir string) error {
		if strings.HasPrefix(dir, "/usr") {
			return errors.New("read-only file system")
		}

		return nil
	}

	results := checkHostOSConfiguration(slog.New(slog.DiscardHandler), deps, "/opt/unbounded").
		Check(context.Background())

	for _, result := range results {
		assert.NotContains(t, result.Message, "/usr/local/bin",
			"a prefixed host must not be probed at the default install directory")
	}
}

func TestCheckExistingDeploymentCleanHost(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.stat = statNotExist()
	deps.outputCmd = outputWith("", errors.New("not found"))

	results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, "").Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckExistingDeploymentDetectsMachineRegistration(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.stat = statNotExist()
	deps.outputCmd = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		if name == "machinectl" && len(args) == 2 && args[0] == "show" && args[1] == "kube2" {
			return "Name=kube2", nil
		}

		return "", errors.New("not found")
	}

	results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, "").Check(context.Background())

	assert.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "kube2", results[0].Target)
	assert.Contains(t, results[0].Message, "registered nspawn machine kube2")
	assert.Contains(t, results[0].Message, "node reset is needed")
	assert.NotContains(t, results[0].Message, "unbounded-agent reset")
}

func TestCheckExistingDeploymentDetectsPartialArtifact(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.stat = statOnlyExists("/var/lib/machines/kube1")
	deps.outputCmd = outputWith("", errors.New("not found"))

	results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, "").Check(context.Background())

	assert.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "/var/lib/machines/kube1", results[0].Target)
	assert.Contains(t, results[0].Message, "nspawn machine rootfs")
	assert.Contains(t, results[0].Message, "node reset is needed")
	assert.NotContains(t, results[0].Message, "unbounded-agent reset")
}

func TestEnsureNoExistingDeploymentReturnsResetInstruction(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.stat = statOnlyExists("/etc/systemd/system/unbounded-agent-daemon.service")
	deps.outputCmd = outputWith("", errors.New("not found"))

	err := ensureNoExistingDeployment(context.Background(), slog.New(slog.DiscardHandler), deps, "")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node reset is needed")
	assert.NotContains(t, err.Error(), "unbounded-agent reset")
	assert.Contains(t, err.Error(), "/etc/systemd/system/unbounded-agent-daemon.service")
}

func TestCheckNSpawnRuntime(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.lookupPath = lookupPathWith(map[string]bool{
		"systemctl":      true,
		"machinectl":     true,
		"systemd-nspawn": true,
	})
	deps.stat = func(string) (fs.FileInfo, error) { return nil, nil }

	results := checkNSpawnRuntime(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.lookupPath = lookupPathWith(map[string]bool{"systemctl": true})
	deps.stat = statMissing()
	results = checkNSpawnRuntime(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Len(t, results, 3)
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Contains(t, results[0].Message, "machinectl")
	assert.Equal(t, preflight.SeverityWarning, results[1].Severity)
	assert.Contains(t, results[1].Message, "systemd-nspawn")
	assert.Equal(t, preflight.SeverityWarning, results[2].Severity)
	assert.Contains(t, results[2].Message, "/run/systemd/system")
}

func TestCheckDockerActive(t *testing.T) {
	testCheckSystemdUnitActive(t, checkDockerActive, dockerServiceUnit)
}

func TestCheckContainerdActive(t *testing.T) {
	testCheckSystemdUnitActive(t, checkContainerdActive, containerdServiceUnit)
}

func TestCheckKubeletActive(t *testing.T) {
	testCheckSystemdUnitActive(t, checkKubeletActive, kubeletServiceUnit)
}

func testCheckSystemdUnitActive(
	t *testing.T,
	check func(*slog.Logger, hostCheckDeps) preflight.Checker,
	wantUnit string,
) {
	t.Helper()

	deps := defaultHostCheckDeps()
	deps.outputCmd = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		assert.Equal(t, "systemctl", name)
		assert.Equal(t, []string{"is-active", wantUnit}, args)

		return "active\n", nil
	}

	results := check(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Equal(t, wantUnit, results[0].Target)

	deps.outputCmd = outputWith("inactive\n", nil)
	results = check(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.outputCmd = outputWith("", errors.New("systemd unavailable"))
	results = check(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Contains(t, results[0].Message, "could not be determined")
}

func TestCheckSwapActive(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.readFile = readFileString("Filename\tType\tSize\tUsed\tPriority\n", nil)

	results := checkSwapActive(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.readFile = readFileString("Filename\tType\tSize\tUsed\tPriority\n/swapfile file 1024 0 -2\n", nil)
	results = checkSwapActive(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)

	deps.readFile = readFileString("", errors.New("missing"))
	results = checkSwapActive(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Contains(t, results[0].Message, "/proc/swaps")
}

func TestCheckDiskSpace(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.statfs = statfsWithFreeBytes(minFreeDiskBytes)

	results := checkDiskSpace(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.statfs = statfsWithFreeBytes(1)
	results = checkDiskSpace(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "/var/lib")
	assert.Contains(t, results[0].Message, "current 0.0 GiB")
	assert.Contains(t, results[0].Message, "required 8.0 GiB")
}

func TestCheckCgroups(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.stat = statExists()

	results := checkCgroups(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.stat = statMissing()
	results = checkCgroups(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "/sys/fs/cgroup")
}

func statfsWithFreeBytes(bytes uint64) func(string, *syscall.Statfs_t) error {
	return func(_ string, stat *syscall.Statfs_t) error {
		stat.Bsize = 1
		stat.Bavail = bytes

		return nil
	}
}

func statExists() func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return nil, nil }
}

func statMissing() func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return nil, errors.New("missing") }
}

func statNotExist() func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist }
}

func statOnlyExists(path string) func(string) (fs.FileInfo, error) {
	return func(candidate string) (fs.FileInfo, error) {
		if candidate == path {
			return nil, nil
		}

		return nil, os.ErrNotExist
	}
}

func packageManagerWithInstalled(installed bool) func(func(string) (string, error)) (*hostPackageManager, error) {
	return func(func(string) (string, error)) (*hostPackageManager, error) {
		return &hostPackageManager{
			name:             "test-package-manager",
			requiredPackages: []string{"systemd-container", "curl"},
			installed: func(context.Context, *slog.Logger, string) bool {
				return installed
			},
		}, nil
	}
}

func lookupPathWith(paths map[string]bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if paths[name] {
			return "/usr/bin/" + name, nil
		}

		return "", exec.ErrNotFound
	}
}

func outputWith(value string, err error) func(context.Context, *slog.Logger, string, ...string) (string, error) {
	return func(context.Context, *slog.Logger, string, ...string) (string, error) {
		return value, err
	}
}

func readFileString(value string, err error) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(value), err }
}

// TestCheckExistingDeploymentDetectsAPrefixedInstall is the safety property
// this check exists for, on a host that configured a prefix.
//
// Bootstrap refuses to run on a host that already carries a deployment. While
// the check looked only at the default prefix, a host installed under a
// configured one looked clean, so bootstrap would provision straight over a
// live install: two daemons, two sets of units, and an ownership record
// describing only the second.
func TestCheckExistingDeploymentDetectsAPrefixedInstall(t *testing.T) {
	const installed = "/opt/unbounded/bin/unbounded-agent-daemon-recovery.sh"

	deps := defaultHostCheckDeps()
	deps.outputCmd = outputWith("", errors.New("not found"))
	deps.stat = func(path string) (os.FileInfo, error) {
		if path == installed {
			return nil, nil //nolint:nilnil // Presence is all this check reads.
		}

		return nil, os.ErrNotExist
	}

	results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, "/opt/unbounded").
		Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, installed)
}

// TestCheckExistingDeploymentDetectsAnAbandonedPrefix covers the other
// direction: the host is being bootstrapped with one prefix but still carries
// files from an earlier install under the default. That is still a dirty host.
func TestCheckExistingDeploymentDetectsAnAbandonedPrefix(t *testing.T) {
	const leftover = "/usr/local/bin/unbounded-agent-daemon-recovery.sh"

	deps := defaultHostCheckDeps()
	deps.outputCmd = outputWith("", errors.New("not found"))
	deps.stat = func(path string) (os.FileInfo, error) {
		if path == leftover {
			return nil, nil //nolint:nilnil // Presence is all this check reads.
		}

		return nil, os.ErrNotExist
	}

	results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, "/opt/unbounded").
		Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, leftover)
}

// TestDeprecatedPreflightEntryPointsKeepTheirSignatures pins the signatures
// these had on main before the installation prefix existed, so callers outside
// this repository keep compiling. The assignments fail to build if a signature
// changes; the names show the wrappers still build the same checks.
func TestDeprecatedPreflightEntryPointsKeepTheirSignatures(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // Exercising the deprecated entry points is the point.
	var (
		checkExisting func(*slog.Logger) preflight.Checker      = CheckExistingDeployment
		ensureNoneYet func(context.Context, *slog.Logger) error = EnsureNoExistingDeployment
		checkHostOS   func(*slog.Logger) preflight.Checker      = CheckHostOSConfiguration
	)

	log := slog.New(slog.DiscardHandler)

	assert.Equal(t, CheckExistingDeploymentName, checkExisting(log).Name())
	assert.Equal(t, checkHostOSConfigurationName, checkHostOS(log).Name())
	assert.NotNil(t, ensureNoneYet)
}
