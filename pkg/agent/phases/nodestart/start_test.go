// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestStartNodeOrdersNVIDIASetupBeforeContainerdAndKubelet(t *testing.T) {
	t.Parallel()

	name := StartNode(slog.New(slog.DiscardHandler), &goalstates.NodeStart{}).Name()
	startNSpawn := strings.Index(name, "start-nspawn-machine")
	setupNVIDIA := strings.Index(name, "setup-nvidia")
	startContainerd := strings.Index(name, "start-containerd")
	startKubelet := strings.Index(name, "start-kubelet")

	require.NotEqual(t, -1, startNSpawn)
	require.NotEqual(t, -1, setupNVIDIA)
	require.NotEqual(t, -1, startContainerd)
	require.NotEqual(t, -1, startKubelet)
	require.Less(t, startNSpawn, setupNVIDIA)
	require.Less(t, setupNVIDIA, startContainerd)
	require.Less(t, startContainerd, startKubelet)
}
