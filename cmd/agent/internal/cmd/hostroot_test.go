// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// TestHostRootPrintsThePlannedRoot pins the output the install script and the
// AgentUpgrade downgrade guard read. The guard compares it with the root the
// running agent resolved, so anything but the bare path on one line refuses
// every upgrade.
func TestHostRootPrintsThePlannedRoot(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	cmd := newCmdHostRoot()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, goalstates.PlannedHostPaths().Root+"\n", out.String())
	assert.True(t, cmd.Hidden, "host-root is for the agent's own tooling, not for operators")
}

func TestHostRootRejectsArguments(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	cmd := newCmdHostRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"extra"})

	require.Error(t, cmd.Execute())
}
