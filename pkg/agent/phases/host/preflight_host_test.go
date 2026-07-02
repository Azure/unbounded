// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

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
	deps.lookupPath = lookupPathWith(map[string]bool{"apt-get": true})

	results := checkHostPackages(slog.New(slog.DiscardHandler), false, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Contains(t, results[0].Message, "systemd-container")
}

func TestCheckHostPackagesBlocksMissingPackagesWhenOfflineArtifactsConfigured(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.lookupPath = lookupPathWith(map[string]bool{"apt-get": true})

	results := checkHostPackages(slog.New(slog.DiscardHandler), true, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "OfflineArtifacts")
	assert.Contains(t, results[0].Message, "systemd-container")
}

func TestCheckHostOSConfiguration(t *testing.T) {
	deps := defaultHostCheckDeps()
	deps.writeProbe = func(string) error { return nil }

	results := checkHostOSConfiguration(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.writeProbe = func(string) error { return errors.New("denied") }
	results = checkHostOSConfiguration(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Len(t, results, 2)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "/etc/sysctl.d")
	assert.Equal(t, preflight.SeverityError, results[1].Severity)
	assert.Contains(t, results[1].Message, "systemd")
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
	deps := defaultHostCheckDeps()
	deps.outputCmd = outputWith("active\n", nil)

	results := checkDockerActive(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)

	deps.outputCmd = outputWith("inactive\n", nil)
	results = checkDockerActive(slog.New(slog.DiscardHandler), deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
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
