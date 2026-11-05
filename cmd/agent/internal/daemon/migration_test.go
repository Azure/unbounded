// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestDaemonStartupRunsLifecycleMigrationBeforeControllerSetup(t *testing.T) {
	t.Parallel()

	op := &fakeNodeOperator{active: &ActiveMachine{
		Name:   "kube1",
		Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
	}}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock")), Migration: testMigrationEnv(t)})
	require.ErrorContains(t, err, "build daemon controller credentials")
	require.Equal(t, 1, op.lifecycleCalls)
}

// TestStartupLockWaitStandsDownWithoutMigration covers a bootstrap that holds
// installation ownership for longer than the daemon is willing to wait.
//
// The daemon must not migrate anything, and it must not report a failure. A
// bootstrap that holds the lock this long is still working, and it starts the
// daemon again when it finishes. Exiting as a failure here is what previously
// drove the unit through its start limit and into OnFailure recovery.
func TestStartupLockWaitStandsDownWithoutMigration(t *testing.T) {
	t.Parallel()
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	lock, err := store.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	op := &fakeNodeOperator{}
	_, _, err = discoverAndMigrate(ctx, discardLogger(), store, op, &migrationEnv{ConfigDir: t.TempDir(), ReprovisionDir: t.TempDir()})
	require.ErrorIs(t, err, ErrDeferred, "waiting out a live bootstrap must defer, not fail")
	require.Zero(t, op.lifecycleCalls)
}

// TestStartupStandsDownWhileInstallationUnfinished covers the other way the
// daemon can find itself with no work: the record says an installation is under
// way and nobody holds the lock, so no bootstrap is running to finish it.
//
// Only a bootstrap run can complete the install, and that run starts the daemon
// on success, so the daemon defers instead of failing.
func TestStartupStandsDownWhileInstallationUnfinished(t *testing.T) {
	t.Parallel()
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))

	record, err := installstate.NewRecord("machine-1", "fingerprint")
	require.NoError(t, err)
	require.NoError(t, store.Save(record))

	op := &fakeNodeOperator{}
	_, _, err = discoverAndMigrate(t.Context(), discardLogger(), store, op, &migrationEnv{ConfigDir: t.TempDir(), ReprovisionDir: t.TempDir()})
	require.ErrorIs(t, err, ErrDeferred, "an unfinished installation must defer, not fail")
	require.Zero(t, op.lifecycleCalls)
}

func TestDaemonStartupFailsLifecycleMigrationWithoutRetry(t *testing.T) {
	t.Parallel()

	resolveErr := errors.New("resolve lifecycle configuration")
	op := &fakeNodeOperator{
		active: &ActiveMachine{
			Name:   "kube1",
			Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
		},
		lifecycleErrs: []error{resolveErr},
	}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock")), Migration: testMigrationEnv(t)})
	require.ErrorIs(t, err, resolveErr)
	require.Equal(t, 1, op.lifecycleCalls)
}

// testMigrationEnv keeps every mode change file in temporary directories and
// never waits for a Node to come back.
func testMigrationEnv(t *testing.T) *migrationEnv {
	t.Helper()

	return &migrationEnv{
		ConfigDir:      t.TempDir(),
		ReprovisionDir: filepath.Join(t.TempDir(), "reprovision"),
		UnitDir:        t.TempDir(),
		NodeReturnWait: time.Millisecond,
		NodeReturnPoll: time.Millisecond,
	}
}

type fixedChecker struct {
	name    string
	results []preflight.Result
}

func (c fixedChecker) Name() string                             { return c.name }
func (c fixedChecker) Check(context.Context) []preflight.Result { return c.results }

var missingNSpawnTool = fixedChecker{name: "nspawn-runtime", results: []preflight.Result{
	preflight.Warning("nspawn-runtime", "nspawn runtime", "nspawn runtime tool is missing and may be installed by bootstrap: %s", "machinectl"),
}}

// An in-place change installs nothing, so a tool the nspawn runtime check only
// warns about must stop it before the source is stopped.
func TestRunMigrationPreflight_WarningsStopAnInPlaceChange(t *testing.T) {
	err := runMigrationPreflight(context.Background(), isolation.ModeNSpawn, true, []preflight.Checker{missingNSpawnTool})
	require.ErrorContains(t, err, "cannot run isolation mode nspawn")
	require.ErrorContains(t, err, "machinectl")
}

// A reprovision bootstraps the target, which installs the tool.
func TestRunMigrationPreflight_WarningsDoNotStopAReprovision(t *testing.T) {
	require.NoError(t, runMigrationPreflight(context.Background(), isolation.ModeNSpawn, false, []preflight.Checker{missingNSpawnTool}))
}

func TestRunMigrationPreflight_ErrorsStopEveryChange(t *testing.T) {
	bad := fixedChecker{name: "x", results: []preflight.Result{preflight.Error("x", "x", "no cgroup v2")}}

	for _, failOnWarnings := range []bool{true, false} {
		require.ErrorContains(t, runMigrationPreflight(context.Background(), isolation.ModeNSpawn, failOnWarnings, []preflight.Checker{bad}), "no cgroup v2")
	}
}

func TestRunMigrationPreflight_AHealthyNSpawnHostPasses(t *testing.T) {
	ok := fixedChecker{name: "nspawn-runtime", results: preflight.ResultsOK("nspawn-runtime", "nspawn runtime", "nspawn runtime is available")}

	require.NoError(t, runMigrationPreflight(context.Background(), isolation.ModeNSpawn, true, []preflight.Checker{ok}))
}

func TestPreflightMigrationTarget_RefusesAModeItDoesNotKnow(t *testing.T) {
	require.ErrorContains(t, preflightMigrationTarget(context.Background(), discardLogger(), "microvm", migrate.Reprovision), "unsupported")
}
