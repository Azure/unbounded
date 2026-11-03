// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// TestEnvtestMachineOperationSelection checks, against a real API server
// serving the MachineOperation CRD, that the daemon's caches hold only the
// operations for its machine and the selector operations, and that the daemon
// falls back to listing every operation when the API server cannot select by
// spec.machineRef.
func TestEnvtestMachineOperationSelection(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	require.NoError(t, machinav1alpha3.AddToScheme(scheme))

	ctx := t.Context()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	// The selector operation selects node-1 by this label.
	require.NoError(t, c.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "a"}},
	}))

	for _, op := range machineOperationsForSelection() {
		require.NoError(t, c.Create(ctx, op))
	}

	selectable, err := MachineRefSelectable(ctx, c, "machine-1")
	require.NoError(t, err)
	require.True(t, selectable)

	ops, err := ListMachineOperations(ctx, c, "machine-1", true)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"own", "selector"}, machineOperationNames(ops))

	t.Run("caches", func(t *testing.T) {
		mgr, err := ctrl.NewManager(cfg, manager.Options{
			Scheme:  scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		require.NoError(t, err)

		caches, err := AddMachineOperationCaches(mgr, "machine-1")
		require.NoError(t, err)

		dispatched := make(chan string, 10)
		reconciler, err := NewMachinaMachineOperationReconcilerWithReader(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			"machine-1",
			"node-1",
			MachineOperationHandlers{
				machinav1alpha3.OperationNodeReboot: func(_ context.Context, _ MachineOperationStore[int64], op MachineOperation) (ctrl.Result, error) {
					dispatched <- op.Name
					return ctrl.Result{}, nil
				},
			},
		)
		require.NoError(t, err)
		reconciler.WatchMachineOperationsIn(caches...)
		require.NoError(t, SetupController("envtest-machine-operation-selection", mgr, reconciler, &recordingReconciler{}))

		mgrCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)

		go func() { done <- mgr.Start(mgrCtx) }()

		t.Cleanup(func() {
			cancel()
			require.NoError(t, <-done)
		})

		var cached [][]string

		for _, cache := range caches {
			require.True(t, cache.WaitForCacheSync(ctx))

			var list machinav1alpha3.MachineOperationList
			require.NoError(t, cache.List(ctx, &list))
			cached = append(cached, machineOperationNames(list.Items))
		}

		assert.Equal(t, [][]string{{"own"}, {"selector"}}, cached)

		var names []string

		for len(names) < 2 {
			select {
			case name := <-dispatched:
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("dispatched %v, want own and selector", names)
			}
		}

		assert.ElementsMatch(t, []string{"own", "selector"}, names)
	})

	t.Run("fallback", func(t *testing.T) {
		// An API server before Kubernetes 1.31, or a CRD from before the field
		// was declared selectable, cannot select by spec.machineRef.
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "machineoperations.unbounded-cloud.io"}, &crd))

		for i := range crd.Spec.Versions {
			crd.Spec.Versions[i].SelectableFields = nil
		}

		require.NoError(t, c.Update(ctx, &crd))

		require.Eventually(t, func() bool {
			selectable, err := MachineRefSelectable(ctx, c, "machine-1")
			return err == nil && !selectable
		}, 30*time.Second, 100*time.Millisecond)

		ops, err := ListMachineOperations(ctx, c, "machine-1", false)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"own", "selector"}, machineOperationNames(ops))
	})
}
