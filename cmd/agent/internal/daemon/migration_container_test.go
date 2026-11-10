// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// An in-place change builds and stops nspawn and container nodes; none has no
// slot to build beside a running node.
func TestNewMigrationBackendKnowsInPlaceModesOnly(t *testing.T) {
	cfg := baseConfig()
	env := &migrationEnv{}

	b, root, err := env.newBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNSpawn, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeNSpawn, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	b, root, err = env.newBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeContainer, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	// The source end only resumes on rollback; the target is activated.
	src, _, err := env.newBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "kube1", Config: cfg, Source: true})
	require.NoError(t, err)
	assert.True(t, src.(*containerMigrationBackend).source)
	assert.False(t, b.(*containerMigrationBackend).source)

	_, _, err = env.newBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNone, Slot: "kube1", Config: cfg})
	require.ErrorContains(t, err, "not supported")

	_, _, err = env.newBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "bogus", Config: cfg})
	require.Error(t, err)
}
