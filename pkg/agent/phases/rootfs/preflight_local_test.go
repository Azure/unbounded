// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

func TestCheckNSpawnMachineProvisioningRestrictiveMachineDir(t *testing.T) {
	gs := validRootFSGoalState(t)
	require.NoError(t, os.Chmod(gs.MachineDir, 0o700))

	results := CheckNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs).Check(context.Background())

	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Equal(t, "machine directory permissions are too restrictive: "+gs.MachineDir, results[0].Message)
}

func TestCheckNSpawnMachineProvisioningCollectsWarningAndError(t *testing.T) {
	gs := validRootFSGoalState(t)
	deps := defaultRootFSCheckDeps()
	deps.stat = func(string) (fs.FileInfo, error) { return fakeRestrictiveDirInfo{}, nil }
	deps.open = func(string) (*os.File, error) { return nil, errors.New("denied") }
	deps.writeProbe = func(string) error { return errors.New("denied") }

	results := checkNSpawnMachineProvisioning(slog.New(slog.DiscardHandler), gs, deps).Check(context.Background())

	require.GreaterOrEqual(t, len(results), 2)
	assert.Equal(t, preflight.SeverityWarning, results[0].Severity)
	assert.Equal(t, preflight.SeverityError, results[1].Severity)
	assert.Equal(t, "machine directory cannot be read: "+gs.MachineDir, results[1].Message)
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
func (fakeDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (fakeDirInfo) IsDir() bool        { return true }
func (fakeDirInfo) Sys() any           { return nil }

type fakeRestrictiveDirInfo struct{}

func (fakeRestrictiveDirInfo) Name() string       { return "dir" }
func (fakeRestrictiveDirInfo) Size() int64        { return 0 }
func (fakeRestrictiveDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o700 }
func (fakeRestrictiveDirInfo) ModTime() time.Time { return time.Time{} }
func (fakeRestrictiveDirInfo) IsDir() bool        { return true }
func (fakeRestrictiveDirInfo) Sys() any           { return nil }
