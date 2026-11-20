// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

// parseFlags runs the command with args and returns the flags it parsed.
func parseFlags(t *testing.T, args ...string) (config, error) {
	t.Helper()

	var parsed config

	cmd := newCommand(func(_ context.Context, cfg config) error {
		parsed = cfg
		return nil
	})
	cmd.SetArgs(args)

	return parsed, cmd.Execute()
}

func TestConfiguredProvidersDefaultsToBuiltIns(t *testing.T) {
	t.Parallel()

	providers, err := configuredProviders("")

	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, unboundedv1alpha3.ExternalProviderAzureVM, providers[0].Name())
}

func TestConfiguredProvidersSelectsOneBuiltIn(t *testing.T) {
	t.Parallel()

	providers, err := configuredProviders(unboundedv1alpha3.ExternalProviderAzureVM)

	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, unboundedv1alpha3.ExternalProviderAzureVM, providers[0].Name())
}

func TestConfiguredProvidersRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := configuredProviders("CustomProvider")

	require.ErrorContains(t, err, `unknown machine-ops provider "CustomProvider"`)
}

func TestLeaderElectionID(t *testing.T) {
	t.Parallel()

	require.Equal(t, "azure-machine-ops-controller", leaderElectionID(config{}))

	siteA := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-a"})
	siteB := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-b"})
	exampleSiteA := leaderElectionID(config{providerName: "ExampleCloud", siteName: "site-a"})

	require.NotEqual(t, "azure-machine-ops-controller", siteA)
	require.NotEqual(t, siteA, siteB)
	require.NotEqual(t, siteA, exampleSiteA)
	require.Equal(t, siteA, leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-a"}))
	require.LessOrEqual(t, len(siteA), 63)
}

func TestLeaderElectionIDForPartialScopes(t *testing.T) {
	t.Parallel()

	providerOnly := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM})
	siteOnly := leaderElectionID(config{siteName: "site-a"})
	both := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-a"})

	for _, id := range []string{providerOnly, siteOnly, both} {
		require.NotEqual(t, controllerName, id)
		require.LessOrEqual(t, len(id), 63)
	}

	require.NotEqual(t, providerOnly, siteOnly)
	require.NotEqual(t, providerOnly, both)
	require.NotEqual(t, siteOnly, both)
}

func TestProviderScopeDoesNotRequireASite(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags(t, "--provider="+unboundedv1alpha3.ExternalProviderAzureVM,
		"--enable-capacity", "--api-server-endpoint=https://example.invalid:443")
	require.NoError(t, err)

	capacity, err := capacityOptions(cfg)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.ExternalProviderAzureVM, capacity.ProviderName)
	require.Empty(t, capacity.SiteName)
}

func TestSafeNamePartFallsBackForSymbolOnlyScope(t *testing.T) {
	t.Parallel()

	part := safeNamePart("!!!")

	require.Contains(t, part, "scope-")
	require.LessOrEqual(t, len("azure-machine-ops-controller-"+part), 63)
}

func TestAzureVMRegistrationDeclaresCapacity(t *testing.T) {
	t.Parallel()

	providers, err := configuredProviders("")
	require.NoError(t, err)

	_, ok := providers[0].Capacity()
	require.True(t, ok)

	for _, kind := range []unboundedv1alpha3.OperationKind{
		unboundedv1alpha3.OperationHostAllocate,
		unboundedv1alpha3.OperationHostRelease,
		unboundedv1alpha3.OperationHostHardReboot,
		unboundedv1alpha3.OperationHostReplace,
	} {
		_, ok := providers[0].Operation(kind)
		require.True(t, ok, kind)
	}
}

func TestCapacityFlagsDefaultOff(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags(t)
	require.NoError(t, err)
	require.False(t, cfg.enableCapacity)
	require.True(t, cfg.leaderElection)
	require.Equal(t, unbounded.SystemNamespace(), cfg.providerConfigNamespace)
	require.Equal(t, unbounded.SystemNamespace(), cfg.credentialSecretNamespace)
	require.Empty(t, cfg.allocationConsumerKinds)

	options, err := capacityOptions(cfg)
	require.NoError(t, err)
	require.Nil(t, options, "existing deployments run no capacity controller")
}

func TestCapacityFlagsEnableAllocationController(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags(t, "--enable-capacity", "--api-server-endpoint=cluster.example.test:443")
	require.NoError(t, err)

	options, err := capacityOptions(cfg)
	require.NoError(t, err)
	require.Equal(t, &machineopscontroller.CapacityOptions{
		CredentialSecretNamespace:  unbounded.SystemNamespace(),
		ProviderConfigNamespace:    unbounded.SystemNamespace(),
		EnableAllocationController: true,
	}, options)
	require.Empty(t, options.AllocationConsumerKinds, "consumer kinds are empty unless asked for")
}

func TestCapacityFlagsCarryScopeNamespacesAndConsumerKinds(t *testing.T) {
	t.Parallel()

	cfg, err := parseFlags(
		t,
		"--enable-capacity", "--api-server-endpoint=cluster.example.test:443",
		"--provider=AzureVM",
		"--site=site-a",
		"--provider-config-namespace=capacity-config",
		"--credential-secret-namespace=capacity-credentials",
		"--allocation-consumer-kind=karpenter.sh/v1/NodeClaim",
		"--allocation-consumer-kind=v1/Node",
	)
	require.NoError(t, err)

	options, err := capacityOptions(cfg)
	require.NoError(t, err)
	require.Equal(t, &machineopscontroller.CapacityOptions{
		ProviderName:               unboundedv1alpha3.ExternalProviderAzureVM,
		SiteName:                   "site-a",
		CredentialSecretNamespace:  "capacity-credentials",
		ProviderConfigNamespace:    "capacity-config",
		EnableAllocationController: true,
		AllocationConsumerKinds: []schema.GroupVersionKind{
			{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"},
			{Version: "v1", Kind: "Node"},
		},
	}, options)
}

func TestCapacityOptionsRejectsInvalidFlags(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args    []string
		message string
	}{
		"leader election disabled": {
			args:    []string{"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--leader-elect=false"},
			message: errCapacityLeaderElection,
		},
		"API server endpoint missing": {
			args:    []string{"--enable-capacity"},
			message: errCapacityAPIServerEndpoint,
		},
		"consumer kind without capacity": {
			args:    []string{"--allocation-consumer-kind=karpenter.sh/v1/NodeClaim"},
			message: errConsumerKindsCapacity,
		},
		"empty provider-config namespace": {
			args:    []string{"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--provider-config-namespace="},
			message: "--provider-config-namespace is required",
		},
		"empty credential namespace": {
			args:    []string{"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--credential-secret-namespace="},
			message: "--credential-secret-namespace is required",
		},
		"malformed consumer kind": {
			args:    []string{"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--allocation-consumer-kind=NodeClaim"},
			message: `"NodeClaim" must be group/version/Kind`,
		},
		"repeated consumer kind": {
			args:    []string{"--enable-capacity", "--api-server-endpoint=cluster.example.test:443", "--allocation-consumer-kind=karpenter.sh/v1/NodeClaim", "--allocation-consumer-kind=karpenter.sh/v1beta1/NodeClaim"},
			message: "repeats NodeClaim.karpenter.sh",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := parseFlags(t, test.args...)
			require.NoError(t, err)

			options, err := capacityOptions(cfg)
			require.ErrorContains(t, err, test.message)
			require.Nil(t, options)
		})
	}
}

func TestParseConsumerKinds(t *testing.T) {
	t.Parallel()

	kinds, err := parseConsumerKinds(nil)
	require.NoError(t, err)
	require.Empty(t, kinds)

	kinds, err = parseConsumerKinds([]string{" karpenter.sh/v1/NodeClaim ", "v1/Node"})
	require.NoError(t, err)
	require.Equal(t, []schema.GroupVersionKind{
		{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"},
		{Version: "v1", Kind: "Node"},
	}, kinds)

	for _, value := range []string{"", "NodeClaim", "karpenter.sh//NodeClaim", "karpenter.sh/v1/", "a/b/c/d"} {
		_, err := parseConsumerKinds([]string{value})
		require.Error(t, err, value)
	}
}

func TestRunRejectsInvalidCapacityFlagsBeforeConnecting(t *testing.T) {
	t.Parallel()

	err := run(context.Background(), config{enableCapacity: true, apiServerEndpoint: "cluster.example.test:443"})
	require.EqualError(t, err, errCapacityLeaderElection)
}
