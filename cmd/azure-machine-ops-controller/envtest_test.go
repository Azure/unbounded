// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// TestEnvtestAddControllers registers the built-in providers on a real
// manager the way run does, with and without the capacity controllers.
func TestEnvtestAddControllers(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	newManager := func(t *testing.T, cfg config) ctrl.Manager {
		t.Helper()

		mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
			Scheme:                  runtimeScheme(),
			LeaderElection:          cfg.leaderElection,
			LeaderElectionID:        leaderElectionID(cfg),
			LeaderElectionNamespace: "default",
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:  "0",
			// Controller names are unique per process, and every subtest
			// registers the same controllers on its own manager.
			Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
		})
		require.NoError(t, err)

		return mgr
	}

	tests := map[string][]string{
		"capacity off":       nil,
		"capacity unscoped":  {"--enable-capacity", "--api-server-endpoint=cluster.example.test:443"},
		"capacity scoped":    {"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--provider=" + unboundedv1alpha3.ExternalProviderAzureVM, "--site=site-a"},
		"capacity consumers": {"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--allocation-consumer-kind=unbounded-cloud.io/v1alpha3/Site"},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseFlags(t, args...)
			require.NoError(t, err)

			capacity, err := capacityOptions(cfg)
			require.NoError(t, err)
			require.Equal(t, cfg.enableCapacity, capacity != nil)

			providers, err := configuredProviders(cfg.providerName)
			require.NoError(t, err)

			require.NoError(t, addControllers(newManager(t, cfg), cfg, providers, capacity))
		})
	}
}
