// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestRootOverridesReplaceNSpawnRoot(t *testing.T) {
	t.Parallel()

	nspawnNode := &NodeStart{MachineName: "kube1", MachineDir: "/var/lib/machines/kube1"}
	require.Equal(t, "/var/lib/machines/kube1/etc/x", nspawnNode.Root().Path("/etc/x"))
	require.False(t, nspawnNode.HostNative())

	override := isolation.DirRoot{SlotName: "v1", Dir: "/tmp/host"}
	hostNode := &NodeStart{MachineName: "kube1", MachineDir: "/var/lib/machines/kube1", RootOverride: override}
	require.Equal(t, "/tmp/host/etc/x", hostNode.Root().Path("/etc/x"))
	require.True(t, hostNode.HostNative())

	rootFS := &RootFS{MachineDir: "/var/lib/machines/kube1", RootOverride: override}
	require.Equal(t, "/tmp/host/etc/x", rootFS.Root().Path("/etc/x"))
	require.Equal(t, "/var/lib/machines/kube1/etc/x", (&RootFS{MachineDir: "/var/lib/machines/kube1"}).Root().Path("/etc/x"))
}

func TestNodeStartExecOverride(t *testing.T) {
	t.Parallel()

	n := &NodeStart{ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
		return args[0] + "!", nil
	}}

	out, err := n.Exec(context.Background(), slog.Default(), "hello")
	require.NoError(t, err)
	require.Equal(t, "hello!", out)
}
