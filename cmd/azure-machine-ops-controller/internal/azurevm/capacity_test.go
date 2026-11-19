// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestRegistrationDeclaresCapacityAndHostOperations(t *testing.T) {
	registration, err := (&Provider{}).Registration()
	require.NoError(t, err)

	_, ok := registration.Capacity()
	require.True(t, ok)

	for _, kind := range []unboundedv1alpha3.OperationKind{
		unboundedv1alpha3.OperationHostAllocate,
		unboundedv1alpha3.OperationHostRelease,
	} {
		operation, ok := registration.Operation(kind)
		require.True(t, ok, kind)
		require.Equal(t, machineops.OperationModeLongRunning, operation.Mode(), kind)
	}

	for kind := range azureVMOperations {
		operation, ok := registration.Operation(kind)
		require.True(t, ok, kind)
		require.Equal(t, machineops.OperationModeImmediate, operation.Mode(), kind)
	}
}

func TestVMNameUsesFullAllocationUID(t *testing.T) {
	name, err := VMName(testAllocationUID)
	require.NoError(t, err)
	require.Equal(t, testVMName, name)

	_, err = VMName("not-a-uid")
	require.Error(t, err)
}

// allocatedVM drives allocate to success and returns the release request
// carrying the owned resources the engine persists from the result.
func allocatedVM(t *testing.T, client *fakeCapacityClient, provider *Provider) machineops.OperationRequest {
	t.Helper()

	offering := resolveTestOffering(t, provider)
	allocate := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, offering)

	begin, err := provider.BeginAllocate(t.Context(), allocate)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Succeeded")

	result, err := provider.PollAllocate(t.Context(), allocate, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)

	release := testOperationRequest(t, unboundedv1alpha3.OperationHostRelease, offering)
	release.Allocation.OwnedResources = result.Result.Allocation.OwnedResources

	return release
}

func pollRelease(t *testing.T, provider *Provider, request machineops.OperationRequest) machineops.PollResult {
	t.Helper()

	result, err := provider.PollRelease(t.Context(), request, machineops.ProviderOperation{
		OperationID: testVMID(),
	})
	require.NoError(t, err)

	return result
}

// Only an Azure answer that the resource or its resource group does not
// exist is absence. Any other 404 is an error.
func TestIsAzureNotFound(t *testing.T) {
	tests := map[string]struct {
		err      error
		notFound bool
	}{
		"resource not found": {
			err:      azureResponseError(http.StatusNotFound, "ResourceNotFound"),
			notFound: true,
		},
		"resource group not found": {
			err:      azureResponseError(http.StatusNotFound, "ResourceGroupNotFound"),
			notFound: true,
		},
		"not found in another case": {
			err:      azureResponseError(http.StatusNotFound, "notfound"),
			notFound: true,
		},
		"wrapped resource not found": {
			err:      fmt.Errorf("get VM: %w", azureResponseError(http.StatusNotFound, "ResourceNotFound")),
			notFound: true,
		},
		"subscription not found": {
			err: azureResponseError(http.StatusNotFound, "SubscriptionNotFound"),
		},
		"404 without an Azure code": {
			err: azureResponseError(http.StatusNotFound, ""),
		},
		"not-found code with another status": {
			err: azureResponseError(http.StatusBadRequest, "ResourceNotFound"),
		},
		"server error": {
			err: azureResponseError(http.StatusInternalServerError, "InternalError"),
		},
		"transport error": {
			err: errors.New("connection reset"),
		},
		"no error": {},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, test.notFound, isAzureNotFound(test.err))
		})
	}
}
