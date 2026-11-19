// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceDeactivationSafety(t *testing.T) {
	notContainer := exec.Command("sh", "-c", "exit 1").Run()
	detectionFailed := exec.Command("sh", "-c", "exit 2").Run()

	require.NoError(t, checkServiceDeactivationSafe("0::/system.slice/agent.service", notContainer))
	require.ErrorContains(t, checkServiceDeactivationSafe("0::/", nil), "run the agent on the host")
	for _, path := range []string{"docker-123.scope", "kubepods.slice", "cri-containerd-123.scope", "libpod-123.scope"} {
		require.ErrorContains(t, checkServiceDeactivationSafe(path, notContainer), "run the agent on the host")
	}
	require.ErrorContains(t, checkServiceDeactivationSafe("0::/", detectionFailed), "detect container execution")
	require.ErrorContains(t, checkServiceDeactivationSafe("0::/", errors.New("missing")), "detect container execution")
}
