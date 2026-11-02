// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMachineOperationHandlersExcludeAllocationOperations(t *testing.T) {
	t.Parallel()

	handlers := (&machineOperationTarget{}).handlers()

	got := make([]string, 0, len(handlers))
	for kind := range handlers {
		got = append(got, string(kind))
	}

	slices.Sort(got)

	want := []string{
		string(v1alpha3.OperationAgentReset),
		string(v1alpha3.OperationAgentUpgrade),
		string(v1alpha3.OperationHostSoftReboot),
		string(v1alpha3.OperationNodeReboot),
	}
	require.Equal(t, want, got)
	require.NotContains(t, handlers, v1alpha3.OperationHostAllocate)
	require.NotContains(t, handlers, v1alpha3.OperationHostRelease)
}
