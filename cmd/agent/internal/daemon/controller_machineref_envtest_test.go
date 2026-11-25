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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

// TestMachineOperationCacheFiltersByMachineRef runs a cache with the options
// the daemon uses against a real API server: the MachineOperation informer
// receives only this machine's operations, so neither another machine's
// operation nor a selector operation ever reaches the cache.
func TestMachineOperationCacheFiltersByMachineRef(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := newScheme()
	direct, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	selectable, err := agentdaemon.MachineRefSelectable(ctx, direct, "m1")
	require.NoError(t, err)
	require.True(t, selectable, "the API server must accept spec.machineRef selection for this test")

	create := func(op *v1alpha3.MachineOperation) {
		t.Helper()
		require.NoError(t, direct.Create(ctx, op))
	}

	create(machineOperationForTest("op-m1", "m1"))
	create(machineOperationForTest("op-m2", "m2"))
	create(&v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-selector"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"site": "site-a"}},
			OperationKind:   v1alpha3.OperationHostPowerOff,
		},
	})

	options := daemonCacheOptions("m1", "node-1", selectable)
	options.Scheme = scheme

	c, err := ctrlcache.New(restConfig, options)
	require.NoError(t, err)

	cacheDone := make(chan struct{})

	go func() {
		defer close(cacheDone)

		_ = c.Start(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-cacheDone
	})

	require.True(t, c.WaitForCacheSync(ctx))

	cachedNames := func() ([]string, error) {
		var ops v1alpha3.MachineOperationList
		if err := c.List(ctx, &ops); err != nil {
			return nil, err
		}

		names := make([]string, len(ops.Items))
		for i := range ops.Items {
			names[i] = ops.Items[i].Name
		}

		return names, nil
	}

	names, err := cachedNames()
	require.NoError(t, err)
	assert.Equal(t, []string{"op-m1"}, names)

	// The cache never holds another machine's operation, not even by name.
	err = c.Get(ctx, client.ObjectKey{Name: "op-m2"}, &v1alpha3.MachineOperation{})
	assert.True(t, apierrors.IsNotFound(err), "cached Get for m2 = %v, want NotFound", err)

	err = c.Get(ctx, client.ObjectKey{Name: "op-selector"}, &v1alpha3.MachineOperation{})
	assert.True(t, apierrors.IsNotFound(err), "cached Get for the selector operation = %v, want NotFound", err)

	// The watch is filtered too: a later m1 operation arrives, while a later
	// m2 operation never does.
	create(machineOperationForTest("op-m1-later", "m1"))
	create(machineOperationForTest("op-m2-later", "m2"))

	require.Eventually(t, func() bool {
		names, err := cachedNames()
		return err == nil && slices.Contains(names, "op-m1-later")
	}, 10*time.Second, 50*time.Millisecond)

	assert.Never(t, func() bool {
		names, err := cachedNames()
		if err != nil {
			return false
		}

		return slices.Contains(names, "op-m2") || slices.Contains(names, "op-m2-later") ||
			slices.Contains(names, "op-selector")
	}, time.Second, 50*time.Millisecond)
}

func machineOperationForTest(name, machineRef string) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}
}
