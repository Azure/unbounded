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
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestDaemonStartupRunsLifecycleMigrationBeforeControllerSetup(t *testing.T) {
	t.Parallel()

	op := &fakeNodeOperator{active: &ActiveMachine{
		Name:   "kube1",
		Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
	}}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))})
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
	_, err = discoverAndMigrate(ctx, discardLogger(), store, op)
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
	_, err = discoverAndMigrate(t.Context(), discardLogger(), store, op)
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
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))})
	require.ErrorIs(t, err, resolveErr)
	require.Equal(t, 1, op.lifecycleCalls)
}

type fixedChecker struct {
	name    string
	results []preflight.Result
}

func (c fixedChecker) Name() string                             { return c.name }
func (c fixedChecker) Check(context.Context) []preflight.Result { return c.results }

// A container host has no machinectl. Bootstrap would install it, so the
// nspawn runtime check only warns; a migration installs nothing, so the missing
// tool must stop it before the source is stopped. e2e showed the migration
// stopping the node, failing to start the target and rolling back instead.
func TestRunMigrationPreflight_NSpawnWarningsStopTheMigration(t *testing.T) {
	missing := fixedChecker{name: "nspawn-runtime", results: []preflight.Result{
		preflight.Warning("nspawn-runtime", "nspawn runtime", "nspawn runtime tool is missing and may be installed by bootstrap: %s", "machinectl"),
	}}

	err := runMigrationPreflight(context.Background(), isolation.ModeNSpawn, []preflight.Checker{missing})
	require.ErrorContains(t, err, "cannot run isolation mode nspawn")
	require.ErrorContains(t, err, "machinectl")
}

func TestRunMigrationPreflight_OtherModesKeepWarningsAsWarnings(t *testing.T) {
	warn := fixedChecker{name: "container-backend", results: []preflight.Result{
		preflight.Warning("container-backend", "container backend", "SELinux is permissive"),
	}}

	require.NoError(t, runMigrationPreflight(context.Background(), isolation.ModeContainer, []preflight.Checker{warn}))
}

func TestRunMigrationPreflight_ErrorsStopEveryMode(t *testing.T) {
	bad := fixedChecker{name: "x", results: []preflight.Result{preflight.Error("x", "x", "no cgroup v2")}}

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeContainer} {
		require.ErrorContains(t, runMigrationPreflight(context.Background(), mode, []preflight.Checker{bad}), "no cgroup v2", mode)
	}
}

func TestRunMigrationPreflight_AHealthyNSpawnHostPasses(t *testing.T) {
	ok := fixedChecker{name: "nspawn-runtime", results: preflight.ResultsOK("nspawn-runtime", "nspawn runtime", "nspawn runtime is available")}

	require.NoError(t, runMigrationPreflight(context.Background(), isolation.ModeNSpawn, []preflight.Checker{ok}))
}
