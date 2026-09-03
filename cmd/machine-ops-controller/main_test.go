// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestConfiguredProvidersDefaultsToBuiltIns(t *testing.T) {
	t.Parallel()

	providers, err := configuredProviders("")

	require.NoError(t, err)
	require.Len(t, providers, 2)
	require.Equal(t, unboundedv1alpha3.ExternalProviderAzureVM, providers[0].Name())
	require.Equal(t, unboundedv1alpha3.ExternalProviderOCIInstance, providers[1].Name())
}

func TestConfiguredProvidersSelectsOneBuiltIn(t *testing.T) {
	t.Parallel()

	providers, err := configuredProviders(unboundedv1alpha3.ExternalProviderOCIInstance)

	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, unboundedv1alpha3.ExternalProviderOCIInstance, providers[0].Name())
}

func TestConfiguredProvidersRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := configuredProviders("CustomProvider")

	require.ErrorContains(t, err, `unknown machine-ops provider "CustomProvider"`)
}

func TestLeaderElectionID(t *testing.T) {
	t.Parallel()

	require.Equal(t, "machine-ops-controller", leaderElectionID(config{}))

	siteA := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-a"})
	siteB := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-b"})
	ociSiteA := leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderOCIInstance, siteName: "site-a"})

	require.NotEqual(t, "machine-ops-controller", siteA)
	require.NotEqual(t, siteA, siteB)
	require.NotEqual(t, siteA, ociSiteA)
	require.Equal(t, siteA, leaderElectionID(config{providerName: unboundedv1alpha3.ExternalProviderAzureVM, siteName: "site-a"}))
	require.LessOrEqual(t, len(siteA), 63)
}

func TestSafeNamePartFallsBackForSymbolOnlyScope(t *testing.T) {
	t.Parallel()

	part := safeNamePart("!!!")

	require.Contains(t, part, "scope-")
	require.LessOrEqual(t, len("machine-ops-controller-"+part), 63)
}
