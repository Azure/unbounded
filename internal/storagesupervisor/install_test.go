// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileModeConfig builds a Config that installs from a local release-layout
// tarball into a temp HostRoot, with systemctl stubbed by the caller.
func fileModeConfig(t *testing.T, hostRoot, archive string) Config {
	t.Helper()

	return Config{
		Repo:        "Azure/unbounded-kube",
		Version:     "v1.0.0",
		Prefix:      "/opt/unbounded-storage",
		ServiceName: "unbounded-storage",
		ConfigPath:  "/etc/unbounded-storage/config.binpb",
		Arch:        "amd64",
		PoolBytes:   defaultPoolBytes,
		Source:      archive,
		SourceMode:  SourceFile,
		HostRoot:    hostRoot,
		Systemctl:   []string{"systemctl"},
	}
}

func TestInstallWithRunnerFileMode(t *testing.T) {
	hostRoot := t.TempDir()
	archive := writeTempTarGz(t, releaseLayoutTar(t))
	cfg := fileModeConfig(t, hostRoot, archive)
	runner := &recordingRunner{failAt: -1}

	require.NoError(t, InstallWithRunner(context.Background(), cfg, runner))

	// Release staged at HostRoot/<prefix>/releases/<version>-<arch>.
	releaseDir := filepath.Join(hostRoot, cfg.Prefix, "releases", "v1.0.0-amd64")
	bin := filepath.Join(releaseDir, "bin", "unbounded-storage")
	info, err := os.Stat(bin)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111)

	// current symlink points at the host-absolute release path (no HostRoot).
	currentLink := filepath.Join(hostRoot, cfg.Prefix, "current")
	target, err := os.Readlink(currentLink)
	require.NoError(t, err)
	assert.Equal(t, "/opt/unbounded-storage/releases/v1.0.0-amd64", target)

	// Unit written under HostRoot.
	unitPath := filepath.Join(hostRoot, systemdUnitDir, "unbounded-storage.service")
	_, err = os.Stat(unitPath)
	require.NoError(t, err)

	// systemctl reload + enable + restart in order.
	require.Len(t, runner.calls, 3)
	assert.Equal(t, []string{"systemctl", "daemon-reload"}, runner.calls[0])
	assert.Equal(t, []string{"systemctl", "enable", "unbounded-storage"}, runner.calls[1])
	assert.Equal(t, []string{"systemctl", "restart", "unbounded-storage"}, runner.calls[2])
}

func TestInstallWithRunnerReinstallFlipsSymlink(t *testing.T) {
	hostRoot := t.TempDir()
	archive := writeTempTarGz(t, releaseLayoutTar(t))
	runner := &recordingRunner{failAt: -1}

	cfgV1 := fileModeConfig(t, hostRoot, archive)
	cfgV1.Version = "v1.0.0"
	require.NoError(t, InstallWithRunner(context.Background(), cfgV1, runner))

	cfgV2 := fileModeConfig(t, hostRoot, archive)
	cfgV2.Version = "v2.0.0"
	require.NoError(t, InstallWithRunner(context.Background(), cfgV2, runner))

	currentLink := filepath.Join(hostRoot, cfgV2.Prefix, "current")
	target, err := os.Readlink(currentLink)
	require.NoError(t, err)
	assert.Equal(t, "/opt/unbounded-storage/releases/v2.0.0-amd64", target)

	// Both release dirs remain on disk.
	for _, v := range []string{"v1.0.0-amd64", "v2.0.0-amd64"} {
		_, err := os.Stat(filepath.Join(hostRoot, cfgV2.Prefix, "releases", v))
		require.NoError(t, err, "release %s should exist", v)
	}
}

func TestValidatePayload(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		err := validatePayload(t.TempDir())
		assert.Error(t, err)
	})

	t.Run("non-executable binary", func(t *testing.T) {
		staging := t.TempDir()
		binDir := filepath.Join(staging, "bin")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "unbounded-storage"), []byte("x"), 0o644))

		err := validatePayload(staging)
		assert.Error(t, err)
	})

	t.Run("valid executable", func(t *testing.T) {
		staging := t.TempDir()
		binDir := filepath.Join(staging, "bin")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "unbounded-storage"), []byte("x"), 0o755))

		assert.NoError(t, validatePayload(staging))
	})
}

func TestFlipCurrentAtomicReplace(t *testing.T) {
	hostRoot := t.TempDir()
	cfg := Config{Prefix: "/opt/unbounded-storage", Version: "v1", Arch: "amd64", HostRoot: hostRoot}

	require.NoError(t, flipCurrent(cfg))

	link := filepath.Join(hostRoot, cfg.Prefix, "current")
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "/opt/unbounded-storage/releases/v1-amd64", target)

	// Flipping again to a new version replaces the link without error.
	cfg.Version = "v2"
	require.NoError(t, flipCurrent(cfg))

	target, err = os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "/opt/unbounded-storage/releases/v2-amd64", target)

	// No leftover temp link.
	_, err = os.Lstat(link + ".tmp")
	assert.True(t, os.IsNotExist(err))
}

func TestInstallWithRunnerInvalidPayload(t *testing.T) {
	hostRoot := t.TempDir()
	// Archive whose binary is not executable should fail validation.
	archive := writeTempTarGz(t, buildTarGz(t, []tarEntry{
		{name: "top/", typ: tar.TypeDir, mode: 0o755},
		{name: "top/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "top/bin/unbounded-storage", body: "x", mode: 0o644},
	}))
	cfg := fileModeConfig(t, hostRoot, archive)
	runner := &recordingRunner{failAt: -1}

	err := InstallWithRunner(context.Background(), cfg, runner)
	require.Error(t, err)
	assert.Empty(t, runner.calls, "systemctl should not run when payload is invalid")
}

func TestPreconditionsEmptySystemctl(t *testing.T) {
	err := Preconditions(Config{Prefix: "/opt/unbounded-storage"})
	require.Error(t, err)
}
