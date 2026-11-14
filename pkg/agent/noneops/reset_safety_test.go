// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestResetMarkerPrecedesEveryUnitStop(t *testing.T) {
	for _, stateDir := range []string{"", "/etc/example-agent"} {
		t.Run(stateDir, func(t *testing.T) {
			td, h, root := newTeardown(t)
			td.StateDir = stateDir

			put(t, root, filepath.Join("etc/systemd/system", none.UnitKubelet), "owned")
			marker := filepath.Join(root, hostnode.OrDefault(stateDir, goalstates.AgentConfigDir), "none-reset")
			stops := 0
			td.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				if name == "systemctl" && len(args) > 0 && args[0] == "stop" {
					stops++

					require.FileExists(t, marker)
				}

				return h.run(ctx, log, name, args...)
			}
			require.NoError(t, td.Stop().Do(t.Context()))
			require.Positive(t, stops)
		})
	}
}

func TestResetMarkerFailureDoesNotStopUnits(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, filepath.Join("etc/systemd/system", none.UnitKubelet), "owned")

	td.StateDir = "/blocked"
	put(t, root, td.StateDir, "not a directory")
	require.Error(t, td.Stop().Do(t.Context()))
	require.Empty(t, h.calls)
}

func TestResetDoesNotDeleteCNIStateAfterFailedDestroy(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, filepath.Join("etc/systemd/system", none.UnitKubelet), "owned")
	reservation := filepath.Join(hostnode.CNIStateDir, "network", "10.1.0.2")
	put(t, root, reservation, "live pod")
	put(t, root, "/mountinfo", "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n")

	h.fail["systemctl stop "+none.UnitKubelet] = errors.New("still running")

	require.Error(t, td.Destroy().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, reservation))
	require.True(t, td.Present())

	for _, call := range h.calls {
		require.False(t, strings.HasPrefix(call, "umount"), call)
	}

	clear(h.fail)
	require.NoError(t, td.Destroy().Do(t.Context()))

	_, err := os.Stat(filepath.Join(root, hostnode.CNIStateDir))
	require.ErrorIs(t, err, os.ErrNotExist)
}
