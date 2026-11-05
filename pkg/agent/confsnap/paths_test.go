// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package confsnap

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestPathsFor(t *testing.T) {
	t.Parallel()

	require.Equal(t, SharedPaths(), PathsFor(), "no mode adds nothing")
	require.Equal(t, SharedPaths(), PathsFor(isolation.ModeNSpawn, isolation.ModeNone))

	// A container slot writes the host's CNI plugins when it is prepared.
	container := PathsFor(isolation.ModeContainer)
	require.Contains(t, container, goalstates.CNIBinDir)
	require.Subset(t, container, SharedPaths())

	// Once, however many modes ask for it.
	both := PathsFor(isolation.ModeNSpawn, isolation.ModeContainer, isolation.ModeContainer)
	require.Len(t, both, len(SharedPaths())+1)
}
