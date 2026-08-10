// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDaemonService struct {
	plan                  ServicePlan
	prepared              []string
	reloads               int
	restarts              int
	restartErr            error
	restartContextErrors  []error
	healthTargets         []string
	failHealthFor         string
	healthFailureCallback func()
}

func (s *fakeDaemonService) Preflight(context.Context, string) (ServicePlan, error) {
	return s.plan, nil
}

func (s *fakeDaemonService) Prepare(_ context.Context, current string) error {
	s.prepared = append(s.prepared, current)
	return nil
}

func (s *fakeDaemonService) Reload(context.Context) error {
	s.reloads++
	return nil
}

func (s *fakeDaemonService) Restart(ctx context.Context) error {
	s.restarts++
	s.restartContextErrors = append(s.restartContextErrors, ctx.Err())

	return s.restartErr
}

func (s *fakeDaemonService) WaitHealthy(_ context.Context, target string) error {
	s.healthTargets = append(s.healthTargets, target)
	if target == s.failHealthFor {
		if s.healthFailureCallback != nil {
			s.healthFailureCallback()
		}

		return errors.New("candidate unhealthy")
	}

	return nil
}

func TestSnapshotActivationCandidatePinsOpenedContent(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "candidate")
	original := []byte("#!/bin/sh\nexit 0\n")
	replacement := []byte("#!/bin/sh\nexit 42\n")

	require.NoError(t, os.WriteFile(sourcePath, original, 0o755))

	snapshotPath, cleanup, err := snapshotActivationCandidate(sourcePath, 0o755)
	require.NoError(t, err)

	defer cleanup()

	require.NoError(t, os.WriteFile(sourcePath, replacement, 0o755))
	assert.Equal(t, original, mustReadFile(t, snapshotPath))
}

func TestAcquireHostActivationLockSerializesCallers(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "activation.lock")

	first, err := AcquireHostActivationLock(lockPath)
	require.NoError(t, err)

	defer first.Close() //nolint:errcheck // test cleanup

	_, err = AcquireHostActivationLock(lockPath)
	require.ErrorIs(t, err, ErrActivationInProgress)

	require.NoError(t, first.Close())

	second, err := AcquireHostActivationLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

func TestPreflightHostDaemonActivationInitialLayoutDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	legacy := []byte("#!/bin/sh\nexit 0\n")
	candidate := []byte("#!/bin/sh\n[ \"$1\" = version ]\n")

	require.NoError(t, os.WriteFile(layout.BinaryPath, legacy, 0o755))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, candidate, 0o755))

	service := &fakeDaemonService{plan: ServicePlan{UpdateRequired: true, Description: "update unit"}}
	plan, err := PreflightHostDaemonActivation(context.Background(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.True(t, plan.InitializeLayout)
	assert.True(t, plan.CandidateChanged)
	assert.Equal(t, layout.GreenPath, plan.TargetPath)
	assert.Equal(t, layout.BluePath, plan.RollbackPath)
	assert.Equal(t, layout.CurrentPath, plan.CurrentLinkPath)
	assert.Equal(t, layout.LastGoodPath, plan.LastGoodLinkPath)
	assert.True(t, plan.Service.UpdateRequired)
	assert.True(t, plan.UpdateLastGood)

	for _, path := range []string{layout.BluePath, layout.GreenPath, layout.CurrentPath, layout.LastGoodPath, filepath.Join(dir, "activation.lock")} {
		_, err := os.Lstat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, path)
	}
}

func TestPreflightHostDaemonActivationRejectsUnstagedCandidate(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	writeExecutable(t, layout.BinaryPath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	_, err := PreflightHostDaemonActivation(context.Background(), ActivationOptions{
		Layout:        layout,
		CandidatePath: layout.BinaryPath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, &fakeDaemonService{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staged separately")
}

func TestPreflightHostDaemonActivationRejectsAliasedTargetSlot(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	writeExecutable(t, layout.GreenPath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.GreenPath, layout.BluePath))
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.GreenPath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	_, err := PreflightHostDaemonActivation(context.Background(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, &fakeDaemonService{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves to active binary")
}

func TestActivateHostDaemonInitializesAndActivatesCandidate(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	legacy := []byte("#!/bin/sh\nexit 0\n")
	candidate := []byte("#!/bin/sh\n[ \"$1\" = version ]\n")

	require.NoError(t, os.WriteFile(layout.BinaryPath, legacy, 0o755))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, candidate, 0o755))

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.True(t, result.Initialized)
	assert.True(t, result.Changed)
	assert.Equal(t, layout.GreenPath, result.CurrentPath)
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.BinaryPath))
	assert.Equal(t, legacy, mustReadFile(t, layout.BluePath))
	assert.Equal(t, candidate, mustReadFile(t, layout.GreenPath))
	assert.Equal(t, 1, service.reloads)
	assert.Equal(t, 1, service.restarts)
	assert.Equal(t, []string{layout.GreenPath}, service.healthTargets)
}

func TestActivateHostDaemonAdoptsCurrentLinkToSingleBinary(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	legacy := []byte("#!/bin/sh\nexit 0\n")
	candidate := []byte("#!/bin/sh\n[ \"$1\" = version ]\n")

	require.NoError(t, os.WriteFile(layout.BinaryPath, legacy, 0o755))
	require.NoError(t, os.Symlink(layout.BinaryPath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.BinaryPath, layout.LastGoodPath))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, candidate, 0o755))

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.True(t, result.Initialized)
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
	assert.Equal(t, legacy, mustReadFile(t, layout.BluePath))
	assert.Equal(t, candidate, mustReadFile(t, layout.GreenPath))
}

func TestActivateHostDaemonRepairsMissingCurrentLink(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	legacy := []byte("#!/bin/sh\nexit 0\n")
	candidate := []byte("#!/bin/sh\n[ \"$1\" = version ]\n")

	require.NoError(t, os.WriteFile(layout.BluePath, legacy, 0o755))
	require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(candidatePath, candidate, 0o755))

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.True(t, result.Initialized)
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
	assert.Equal(t, legacy, mustReadFile(t, layout.BluePath))
	assert.Equal(t, candidate, mustReadFile(t, layout.GreenPath))
}

func TestActivateHostDaemonSwitchesExistingLayout(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	writeExecutable(t, layout.BluePath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.False(t, result.Initialized)
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
}

func TestActivateHostDaemonRejectsDirectoryDestinationDuringPreflight(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	previousLastGood := filepath.Join(dir, "previous-last-good")

	writeExecutable(t, layout.BluePath, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, previousLastGood, "#!/bin/sh\nexit 0\n# previous\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(previousLastGood, layout.LastGoodPath))

	layout.BinaryPath = filepath.Join(dir, "compatibility-directory")
	require.NoError(t, os.Mkdir(layout.BinaryPath, 0o755))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	service := &fakeDaemonService{}
	_, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a replaceable regular file or symlink")

	assert.Empty(t, service.prepared)
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, previousLastGood, mustEvalSymlinks(t, layout.LastGoodPath))
}

func TestActivateHostDaemonAllowsMissingCompatibilityPath(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	layout.BinaryPath = ""
	writeExecutable(t, layout.BluePath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, &fakeDaemonService{})
	require.NoError(t, err)

	assert.True(t, result.Changed)
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
}

func TestActivateHostDaemonIdenticalCandidatePreservesLastGood(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	active := "#!/bin/sh\n[ \"$1\" = version ]\n"
	writeExecutable(t, layout.BluePath, active)
	writeExecutable(t, layout.GreenPath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.GreenPath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, active)

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.False(t, result.Changed)
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.GreenPath, mustEvalSymlinks(t, layout.LastGoodPath))
}

func TestActivateHostDaemonIdenticalCandidateRepairsMissingLastGood(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	active := "#!/bin/sh\n[ \"$1\" = version ]\n"
	writeExecutable(t, layout.BluePath, active)
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, active)

	service := &fakeDaemonService{}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.NoError(t, err)

	assert.False(t, result.Changed)
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.LastGoodPath))
}

func TestActivateHostDaemonRollsBackUnhealthyCandidate(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	writeExecutable(t, layout.BluePath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	ctx, cancel := context.WithCancel(context.Background())
	service := &fakeDaemonService{
		failHealthFor:         layout.GreenPath,
		healthFailureCallback: cancel,
	}
	result, err := ActivateHostDaemon(ctx, discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.Error(t, err)

	assert.True(t, result.RolledBack)
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, 2, service.restarts)
	assert.Equal(t, []error{nil, nil}, service.restartContextErrors)
	assert.Equal(t, []string{layout.GreenPath, layout.BluePath}, service.healthTargets)
}

func TestActivateHostDaemonReportsUnsuccessfulRollback(t *testing.T) {
	dir := t.TempDir()
	layout := testActivationLayout(dir)
	writeExecutable(t, layout.BluePath, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
	require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))
	require.NoError(t, os.Symlink(layout.CurrentPath, layout.BinaryPath))

	candidatePath := filepath.Join(dir, "candidate")
	writeExecutable(t, candidatePath, "#!/bin/sh\n[ \"$1\" = version ]\n")

	service := &fakeDaemonService{restartErr: errors.New("restart failed")}
	result, err := ActivateHostDaemon(context.Background(), discardLogger(), ActivationOptions{
		Layout:        layout,
		CandidatePath: candidatePath,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	require.Error(t, err)

	assert.False(t, result.RolledBack)
	assert.Equal(t, layout.BluePath, mustEvalSymlinks(t, layout.CurrentPath))
	assert.Equal(t, 2, service.restarts)
}

func testActivationLayout(dir string) Layout {
	return Layout{
		BinaryPath:   filepath.Join(dir, "unbounded-agent"),
		BluePath:     filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:    filepath.Join(dir, "unbounded-agent-green"),
		CurrentPath:  filepath.Join(dir, "unbounded-agent-current"),
		LastGoodPath: filepath.Join(dir, "unbounded-agent-last-good"),
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	return resolved
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
