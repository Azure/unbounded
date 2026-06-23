// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingRunner captures every argv passed to Run and can be made to fail on
// a chosen invocation index.
type recordingRunner struct {
	calls   [][]string
	failAt  int
	failErr error
}

func (r *recordingRunner) Run(_ context.Context, argv []string) error {
	r.calls = append(r.calls, argv)

	if r.failErr != nil && len(r.calls)-1 == r.failAt {
		return r.failErr
	}

	return nil
}

func TestSystemctlArgs(t *testing.T) {
	cfg := Config{Systemctl: []string{"nsenter", "-t", "1", "-m", "systemctl"}}

	got := systemctlArgs(cfg, "enable", "unbounded-storage")
	assert.Equal(t, []string{"nsenter", "-t", "1", "-m", "systemctl", "enable", "unbounded-storage"}, got)
}

func TestReloadAndStartEnables(t *testing.T) {
	cfg := Config{Systemctl: []string{"systemctl"}, ServiceName: "unbounded-storage"}
	runner := &recordingRunner{failAt: -1}

	require.NoError(t, reloadAndStart(context.Background(), cfg, runner))

	require.Len(t, runner.calls, 3)
	assert.Equal(t, []string{"systemctl", "daemon-reload"}, runner.calls[0])
	assert.Equal(t, []string{"systemctl", "enable", "unbounded-storage"}, runner.calls[1])
	assert.Equal(t, []string{"systemctl", "restart", "unbounded-storage"}, runner.calls[2])
}

func TestReloadAndStartNoEnable(t *testing.T) {
	cfg := Config{Systemctl: []string{"systemctl"}, ServiceName: "unbounded-storage", NoEnable: true}
	runner := &recordingRunner{failAt: -1}

	require.NoError(t, reloadAndStart(context.Background(), cfg, runner))

	require.Len(t, runner.calls, 1)
	assert.Equal(t, []string{"systemctl", "daemon-reload"}, runner.calls[0])
}

func TestReloadAndStartPropagatesError(t *testing.T) {
	cfg := Config{Systemctl: []string{"systemctl"}, ServiceName: "unbounded-storage"}
	sentinel := errors.New("boom")

	// Fail on the first call (daemon-reload).
	runner := &recordingRunner{failAt: 0, failErr: sentinel}
	err := reloadAndStart(context.Background(), cfg, runner)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Len(t, runner.calls, 1)

	// Fail on the enable call (index 1); restart must not run.
	runner = &recordingRunner{failAt: 1, failErr: sentinel}
	err = reloadAndStart(context.Background(), cfg, runner)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Len(t, runner.calls, 2)
}

func TestRenderUnit(t *testing.T) {
	cfg := Config{
		Repo:        "Azure/unbounded-kube",
		Prefix:      "/opt/unbounded-storage",
		ConfigPath:  "/etc/unbounded-storage/config.binpb",
		ServiceName: "unbounded-storage",
		PoolBytes:   defaultPoolBytes,
	}

	unit := renderUnit(cfg)

	assert.Contains(t, unit, "ExecStart=/opt/unbounded-storage/current/bin/unbounded-storage --config /etc/unbounded-storage/config.binpb")
	assert.Contains(t, unit, "Environment=LD_LIBRARY_PATH=/opt/unbounded-storage/current/lib")
	assert.Contains(t, unit, "Documentation=https://github.com/Azure/unbounded-kube")
	assert.Contains(t, unit, "LimitMEMLOCK=infinity")
	assert.Contains(t, unit, "Restart=always")
	assert.Contains(t, unit, "WantedBy=multi-user.target")
	// No extra storage args appended.
	assert.NotContains(t, unit, "config.binpb --")
}

func TestRenderUnitWithStorageArgs(t *testing.T) {
	cfg := Config{
		Repo:        "Azure/unbounded-kube",
		Prefix:      "/opt/unbounded-storage",
		ConfigPath:  "/etc/unbounded-storage/config.binpb",
		StorageArgs: "--log-level debug",
	}

	unit := renderUnit(cfg)

	assert.Contains(t, unit, "ExecStart=/opt/unbounded-storage/current/bin/unbounded-storage --config /etc/unbounded-storage/config.binpb --log-level debug")
}

func TestRenderUnitHugepageOverride(t *testing.T) {
	cfg := Config{
		Repo:       "Azure/unbounded-kube",
		Prefix:     "/opt/unbounded-storage",
		ConfigPath: "/etc/unbounded-storage/config.binpb",
		PoolBytes:  defaultPoolBytes,
		Hugepages:  256,
	}

	unit := renderUnit(cfg)
	assert.Contains(t, unit, "want=256")
}

func TestWriteUnit(t *testing.T) {
	hostRoot := t.TempDir()
	cfg := Config{
		Repo:        "Azure/unbounded-kube",
		Prefix:      "/opt/unbounded-storage",
		ConfigPath:  "/etc/unbounded-storage/config.binpb",
		ServiceName: "unbounded-storage",
		PoolBytes:   defaultPoolBytes,
		HostRoot:    hostRoot,
	}

	path, err := writeUnit(cfg)
	require.NoError(t, err)

	want := filepath.Join(hostRoot, systemdUnitDir, "unbounded-storage.service")
	assert.Equal(t, want, path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(body), "[Unit]"))
}
