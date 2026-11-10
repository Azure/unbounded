// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestCheckIsolationEnabled(t *testing.T) {
	t.Parallel()

	// Only container is experimental; the other modes pass either way.
	for _, mode := range []v1alpha3.IsolationMode{"", v1alpha3.IsolationNSpawn, v1alpha3.IsolationNone} {
		require.NoError(t, CheckIsolationEnabled(mode, false, "metalman", "pass --flag"), mode)
		require.NoError(t, CheckIsolationEnabled(mode, true, "metalman", "pass --flag"), mode)
	}

	require.NoError(t, CheckIsolationEnabled(v1alpha3.IsolationContainer, true, "metalman", "pass --flag"))

	err := CheckIsolationEnabled(v1alpha3.IsolationContainer, false, "metalman", "pass --flag")
	require.EqualError(t, err, `spec.isolation "container" is experimental and not enabled on this metalman; pass --flag to use it`)
}
