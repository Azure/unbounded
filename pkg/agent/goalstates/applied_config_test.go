// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// nspawn keeps the file names every agent reads; other modes never use them, so
// an agent that predates a mode cannot mistake its node for an nspawn machine.
func TestAppliedConfigName(t *testing.T) {
	require.Equal(t, "kube1", AppliedConfigName("", "kube1"))
	require.Equal(t, "kube2", AppliedConfigName(isolation.ModeNSpawn, "kube2"))
	require.Equal(t, "none-kube1", AppliedConfigName(isolation.ModeNone, "kube1"))
	require.Equal(t, AgentConfigDir+"/none-kube1-applied-config.json", AppliedConfigPath(AppliedConfigName(isolation.ModeNone, "kube1")))
}

func TestAppliedConfigLocations(t *testing.T) {
	require.Equal(t, []AppliedConfigLocation{
		{Name: "kube1", Slot: "kube1", Mode: isolation.ModeNSpawn},
		{Name: "kube2", Slot: "kube2", Mode: isolation.ModeNSpawn},
		{Name: "none-kube1", Slot: "kube1", Mode: isolation.ModeNone},
	}, AppliedConfigLocations())
}
