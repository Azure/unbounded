// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckNSpawnMachineProvisioningMissingMachineDir(t *testing.T) {
	gs := validRootFSGoalState(t)
	gs.MachineDir = filepath.Join(t.TempDir(), "missing")
	deps := defaultRootFSCheckDeps()
	deps.writeProbe = func(string) error { return nil }

	results := checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckNSpawnMachineProvisioningMissingParentDir(t *testing.T) {
	base := t.TempDir()
	gs := validRootFSGoalState(t)
	gs.MachineDir = filepath.Join(base, "var", "lib", "machines", "kube1")

	results := CheckNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs).Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckNSpawnMachineProvisioningNonEmptyMachineDir(t *testing.T) {
	gs := validRootFSGoalState(t)
	require.NoError(t, os.WriteFile(filepath.Join(gs.MachineDir, "file"), []byte("x"), 0o600))

	deps := defaultRootFSCheckDeps()
	deps.writeProbe = func(string) error { return nil }

	results := checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckNSpawnMachineProvisioningMachineDirFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	gs := validRootFSGoalState(t)
	gs.MachineDir = path

	results := CheckNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
}

func TestCheckNSpawnMachineProvisioningWritablePaths(t *testing.T) {
	gs := validRootFSGoalState(t)
	deps := defaultRootFSCheckDeps()
	deps.writeProbe = func(string) error { return nil }

	results := checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)

	deps.writeProbe = func(string) error { return errors.New("denied") }
	results = checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
}

func TestCheckNSpawnMachineProvisioningUnreadableMachineDir(t *testing.T) {
	gs := validRootFSGoalState(t)
	deps := defaultRootFSCheckDeps()
	deps.stat = func(string) (fs.FileInfo, error) { return fakeDirInfo{}, nil }
	deps.open = func(string) (*os.File, error) { return nil, errors.New("denied") }

	results := checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
}

type fakeDirInfo struct{}

func (fakeDirInfo) Name() string       { return "dir" }
func (fakeDirInfo) Size() int64        { return 0 }
func (fakeDirInfo) Mode() fs.FileMode  { return fs.ModeDir }
func (fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (fakeDirInfo) IsDir() bool        { return true }
func (fakeDirInfo) Sys() any           { return nil }
