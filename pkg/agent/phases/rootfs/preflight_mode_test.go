// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func names(checks []preflight.Checker) []string {
	var out []string
	for _, c := range checks {
		out = append(out, c.Name())
	}

	return out
}

func TestPreflightNoneOmitsImageAndMachineChecks(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{}}
	log := slog.New(slog.DiscardHandler)

	nspawn := names(Preflight(log, config.AgentConfig{}, gs))
	require.Contains(t, nspawn, checkOCIImageReachableName)
	require.Len(t, nspawn, 5)
	require.Equal(t, nspawn, names(Preflight(log, config.AgentConfig{Isolation: "nspawn"}, gs)))

	host := names(Preflight(log, config.AgentConfig{Isolation: "none"}, gs))
	require.NotContains(t, host, checkOCIImageReachableName)
	require.Len(t, host, 3, "Kubernetes, CRI and CNI artifacts remain")

	gs.RootFS.LocalDNS.Enabled = true
	require.Len(t, Preflight(log, config.AgentConfig{Isolation: "none"}, gs), 4, "the CoreDNS artifact is still checked")
}

func TestPreflightContainerKeepsImageCheckAndOmitsMachineCheck(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{}}
	log := slog.New(slog.DiscardHandler)

	got := names(Preflight(log, config.AgentConfig{Isolation: "container"}, gs))
	require.Contains(t, got, checkOCIImageReachableName, "the container is built from the same OS image")
	require.NotContains(t, got, checkNSpawnMachineProvisioningName)
	require.Len(t, got, 4, "image, Kubernetes, CRI and CNI artifacts")
}
