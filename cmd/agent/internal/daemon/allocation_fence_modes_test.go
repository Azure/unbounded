// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// makeAllocationHost makes the harness's active node an allocation host.
func makeAllocationHost(h *migHarness) {
	h.active.Config.Kubelet.Configuration = map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + testAllocationUID}
}

// guardedChangeFixture is changeFixture with the allocation launch guard wired
// to reader.
func guardedChangeFixture(t *testing.T, h *migHarness, declared v1alpha3.IsolationMode, version string, allow bool, reader client.Reader) (*repaveReconciler, *fakeNodeOperator, *fakeMigrator, client.Client) {
	t.Helper()

	r, op, mig, c := changeFixture(t, h, declared, version, allow, healthyClient(t))
	r.apiReader = reader
	r.allocationLaunch = testAllocationLaunchOptions(t)

	return r, op, mig, c
}

// A repave that changes the isolation mode is checked before the change is
// decided, so a fence cannot be escaped by a mode change.
func TestReconcileRepave_AllocationLaunchModeChange(t *testing.T) {
	fenced := func() client.Reader { return fakeStatusClient(fencedAllocationMachine()) }
	bound := func() client.Reader { return fakeStatusClient(allocationMachine(testAllocationUID)) }

	for _, pair := range []struct {
		from     isolation.Mode
		declared v1alpha3.IsolationMode
	}{
		{isolation.ModeNSpawn, v1alpha3.IsolationNone},
		{isolation.ModeNone, v1alpha3.IsolationNSpawn},
	} {
		prefix := string(pair.from) + " to " + string(pair.declared) + "/"

		t.Run(prefix+"fenced", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			makeAllocationHost(h)
			r, op, mig, c := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, true, fenced())

			res, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Equal(t, reconcile.Result{}, res)
			assert.Empty(t, mig.strategies, "no handoff is staged")
			assert.Nil(t, getChange(t, c), "no isolation change is reported")
			assert.False(t, op.restartCalled)
			assert.Nil(t, op.repaveConfig)
		})

		t.Run(prefix+"transient budget", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			makeAllocationHost(h)

			var reads atomic.Int32

			reader := countingReader(nil, &reads, repeat(apierrors.NewServiceUnavailable("down"), 100000))
			r, _, mig, _ := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, true, reader)

			res, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Positive(t, res.RequeueAfter)
			assert.Empty(t, mig.strategies)
		})

		t.Run(prefix+"allowed", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			makeAllocationHost(h)
			r, _, mig, _ := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, true, bound())

			_, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Equal(t, []migrate.Strategy{migrate.Reprovision}, mig.strategies)
		})

		// Without the opt-in the change is not made, and a repave with no other
		// drift would restart the node stack to register it again.
		t.Run(prefix+"fenced kept-mode restart", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			makeAllocationHost(h)
			r, op, mig, _ := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, false, fenced())

			_, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.False(t, op.restartCalled)
			assert.Nil(t, op.repaveConfig)
			assert.Empty(t, mig.strategies)
		})

		t.Run(prefix+"stale cache", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			makeAllocationHost(h)
			r, op, mig, c := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, true, fenced())

			// The cache shows the Machine bound and unfenced.
			cached := getMachine(t, c)
			cached.Spec.AllocationRef = &v1alpha3.UIDReference{Name: "alloc-1", UID: testAllocationUID}
			require.NoError(t, c.Update(t.Context(), &cached))

			_, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Empty(t, mig.strategies)
			assert.False(t, op.restartCalled)
		})

		t.Run(prefix+"non-allocation host", func(t *testing.T) {
			h := newMigHarness(t, pair.from)
			r, _, mig, _ := guardedChangeFixture(t, h, pair.declared, h.active.Config.Cluster.Version, true, failingReader{t: t})

			_, err := r.ReconcileRepave(t.Context(), "node-delete")
			require.NoError(t, err)
			assert.Equal(t, []migrate.Strategy{migrate.Reprovision}, mig.strategies, "the change goes ahead as before")
		})
	}

	t.Run("none with drift and no mode change, fenced", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNone)
		makeAllocationHost(h)
		r, op, mig, _ := guardedChangeFixture(t, h, "", "1.34.9", false, fenced())

		res, err := r.ReconcileRepave(t.Context(), "node-delete")
		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, res)
		assert.Nil(t, op.repaveConfig, "RepaveNode is not called")
		assert.Empty(t, mig.strategies)
	})
}

// With no drift and no declared mode change nothing starts, so nothing is read.
func TestReconcileRepave_AllocationLaunchNoDriftMakesNoRead(t *testing.T) {
	cfg := allocationConfig(t, testAllocationUID)
	machine := fencedAllocationMachine()

	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: cfg}}
	reconciler := &repaveReconciler{
		installation:     installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:           fakeStatusClient(machine),
		apiReader:        failingReader{t: t},
		allocationLaunch: testAllocationLaunchOptions(t),
		log:              discardLogger(),
		machineName:      "test-machine",
		nodeOperator:     op,
	}

	res, err := reconciler.ReconcileRepave(context.Background(), "node-delete")
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, res)
	assert.Nil(t, op.repaveConfig)
	assert.False(t, op.restartCalled)
}
