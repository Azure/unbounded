// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
	"github.com/Azure/unbounded/pkg/machineops/controller"
)

func TestPublicControllerTypesAreReusable(t *testing.T) {
	t.Parallel()

	provider := testProvider{name: "ExampleProvider"}
	reconciler := &controller.MachineOperationReconciler{
		Providers:    []machineops.Provider{provider},
		SiteName:     "site-a",
		ProviderName: provider.Name(),
	}

	require.Equal(t, "site-a", reconciler.SiteName)
	require.Equal(t, provider.Name(), reconciler.ProviderName)
	require.Len(t, reconciler.Providers, 1)
}

type testProvider struct {
	name string
}

func (p testProvider) Name() string {
	return p.name
}

func (testProvider) Supports(unboundedv1alpha3.OperationKind) bool {
	return true
}

func (testProvider) Execute(
	_ context.Context,
	_ machineops.OperationRequest,
) (machineops.OperationResult, error) {
	return machineops.OperationResult{}, nil
}

func (testProvider) Cleanup(
	_ context.Context,
	_ machineops.OperationRequest,
	_ machineops.OperationResult,
) error {
	return nil
}
