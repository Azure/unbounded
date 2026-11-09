// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestPackageSetForNSpawnIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{"", isolation.ModeNSpawn} {
		set := packageSetFor(mode)
		assert.Equal(t, debianRequiredPackages, set.debian)
		assert.Equal(t, rpmRequiredPackages, set.rpm)
		assert.Contains(t, set.debian, "systemd-container")
	}
}

func TestPackageSetForOtherModesDropsSystemdContainer(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNone} {
		set := packageSetFor(mode)
		assert.NotContains(t, set.debian, "systemd-container")
		assert.NotContains(t, set.rpm, "systemd-container")
		assert.ElementsMatch(t, []string{"curl", "nftables", "util-linux"}, set.debian)
	}

	// The package lists themselves are not mutated.
	assert.Contains(t, debianRequiredPackages, "systemd-container")
}

func TestDetectHostPackageManagerModeSelectsPackages(t *testing.T) {
	t.Parallel()

	pm, err := detectHostPackageManager(existingPathLookup("apt-get"), false, isolation.ModeNone)
	require.NoError(t, err)
	assert.NotContains(t, pm.requiredPackages, "systemd-container")

	pm, err = detectHostPackageManager(existingPathLookup("apt-get"), false, isolation.ModeNSpawn)
	require.NoError(t, err)
	assert.Contains(t, pm.requiredPackages, "systemd-container")

	pm, err = detectHostPackageManager(existingPathLookup("dnf"), false, isolation.ModeNone)
	require.NoError(t, err)
	assert.NotContains(t, pm.requiredPackages, "systemd-container")
}

// A host with the tools but no systemd-nspawn is usable for none and not for
// nspawn, with or without a package manager.
func TestCapabilityOnlyHostWithoutNSpawnPerMode(t *testing.T) {
	t.Parallel()

	lookup := existingPathLookup("curl", "nft", "mountpoint")

	_, err := detectHostPackageManager(lookup, false, isolation.ModeNSpawn)
	require.ErrorContains(t, err, "systemd-container")

	pm, err := detectHostPackageManager(lookup, false, isolation.ModeNone)
	require.NoError(t, err)
	assert.Equal(t, "none", pm.name)

	_, err = detectHostPackageManager(lookup, true, isolation.ModeNSpawn)
	require.ErrorContains(t, err, "systemd-container")

	_, err = detectHostPackageManager(lookup, true, isolation.ModeNone)
	require.NoError(t, err)

	// A tool none still needs is still reported.
	_, err = detectHostPackageManager(existingPathLookup("curl", "mountpoint"), true, isolation.ModeNone)
	require.ErrorContains(t, err, "nftables")
}

func TestInstallPackagesForNoneIgnoresMissingSystemdContainer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	// No package manager and no tools: the error lists what none needs, and
	// never systemd-container.
	err := InstallPackagesFor(slog.New(slog.DiscardHandler), isolation.ModeNone).Do(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "systemd-container")

	err = InstallPackagesFor(slog.New(slog.DiscardHandler), isolation.ModeNSpawn).Do(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "systemd-container")
}

func checkNames(checks []preflight.Checker) []string {
	var names []string
	for _, c := range checks {
		names = append(names, c.Name())
	}

	return names
}

func TestPreflightChecksDependOnMode(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	nspawn := checkNames(Preflight(log, config.AgentConfig{}, nil))
	assert.Contains(t, nspawn, checkNSpawnRuntimeName)
	assert.NotContains(t, nspawn, checkNoneBackendName)
	assert.Equal(t, nspawn, checkNames(Preflight(log, config.AgentConfig{Isolation: "nspawn"}, nil)))

	noneChecks := checkNames(Preflight(log, config.AgentConfig{Isolation: "none"}, nil))
	assert.NotContains(t, noneChecks, checkNSpawnRuntimeName)
	assert.Contains(t, noneChecks, checkNoneBackendName)
	assert.Contains(t, noneChecks, CheckExistingDeploymentName, "the clean-host check stays so a resumed install can filter it")
	assert.Contains(t, noneChecks, checkContainerdActiveName)
	assert.Contains(t, noneChecks, checkKubeletActiveName)

	// Same checks otherwise, in the same order.
	var without []string

	for _, n := range noneChecks {
		if n != checkNoneBackendName {
			without = append(without, n)
		}
	}

	var nspawnWithout []string

	for _, n := range nspawn {
		if n != checkNSpawnRuntimeName {
			nspawnWithout = append(nspawnWithout, n)
		}
	}

	assert.Equal(t, nspawnWithout, without)
}

func TestPreflightReportsUnknownMode(t *testing.T) {
	t.Parallel()

	checks := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{Isolation: "bogus"}, nil)
	require.Len(t, checks, 1)

	results := checks[0].Check(t.Context())
	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "bogus")
}

func TestCheckNoneBackendRefusesUnmanagedKubeletWithSuggestedMode(t *testing.T) {
	t.Parallel()

	units := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(units, "kubelet.service"), nil, 0o644))

	b := &none.Backend{Host: &none.HostFS{UnitDirs: []string{units}, BinDirs: nil, Proc: filepath.Join(t.TempDir(), "proc")}, Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}}
	results := checkNoneBackend(slog.New(slog.DiscardHandler), b).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "kubelet unit")
	assert.Contains(t, results[0].Message, "use isolation mode nspawn", "invariant 5: fail fast with a suggested mode")
}

func TestCheckNoneBackendRefusesUnmanagedContainerdProcess(t *testing.T) {
	t.Parallel()

	proc := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "321"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "321", "comm"), []byte("containerd\n"), 0o644))

	b := &none.Backend{Host: &none.HostFS{Proc: proc}, Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}}
	results := checkNoneBackend(slog.New(slog.DiscardHandler), b).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "containerd is already running as pid 321")
}

func TestCheckNoneBackendPassesOnCleanHost(t *testing.T) {
	t.Parallel()

	b := &none.Backend{Host: &none.HostFS{UnitDirs: []string{t.TempDir()}, Proc: filepath.Join(t.TempDir(), "proc")}, Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}, CheckPaths: []string{t.TempDir()}}
	results := checkNoneBackend(slog.New(slog.DiscardHandler), b).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckNoneBackendConstructor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, checkNoneBackendName, CheckNoneBackend(slog.New(slog.DiscardHandler)).Name())
}

func TestExistingDeploymentForNoneFindsHostNativeArtifacts(t *testing.T) {
	t.Parallel()

	for _, path := range []string{none.DefaultBase, "/etc/systemd/system/" + none.UnitKubelet, "/etc/systemd/system/" + none.UnitContainerd, "/etc/systemd/system/" + none.UnitLocalDNS} {
		deps := defaultHostCheckDeps()
		deps.stat = statOnlyExists(path)
		deps.outputCmd = outputWith("", errors.New("not found"))

		results := checkExistingDeployment(slog.New(slog.DiscardHandler), deps, isolation.ModeNone).Check(context.Background())
		require.Len(t, results, 1, path)
		assert.Equal(t, preflight.SeverityError, results[0].Severity)
		assert.Equal(t, path, results[0].Target)
		assert.Contains(t, results[0].Message, "node reset is needed")

		// The default mode does not look for them, so its output is unchanged.
		results = checkExistingDeployment(slog.New(slog.DiscardHandler), deps, isolation.DefaultMode).Check(context.Background())
		assert.Equal(t, preflight.SeverityOK, results[0].Severity, path)
	}
}

func TestEnsureNoExistingDeploymentForNone(t *testing.T) {
	t.Parallel()

	deps := defaultHostCheckDeps()
	deps.stat = statOnlyExists(none.DefaultBase)
	deps.outputCmd = outputWith("", errors.New("not found"))

	err := ensureNoExistingDeployment(context.Background(), slog.New(slog.DiscardHandler), deps, isolation.ModeNone)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), none.DefaultBase))

	deps.stat = statNotExist()
	require.NoError(t, ensureNoExistingDeployment(context.Background(), slog.New(slog.DiscardHandler), deps, isolation.ModeNone))
}

func TestCheckNoneBackendRefusesReadOnlyPaths(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "etc")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	// A path that is not a writable directory fails the same way a read-only
	// /etc does.
	b := &none.Backend{Host: &none.HostFS{UnitDirs: []string{t.TempDir()}, Proc: filepath.Join(t.TempDir(), "proc")}, Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}, CheckPaths: []string{file}}
	results := checkNoneBackend(slog.New(slog.DiscardHandler), b).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "not a directory")
}
