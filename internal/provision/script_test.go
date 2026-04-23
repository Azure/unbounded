// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnboundedAgentInstallScript(t *testing.T) {
	t.Parallel()

	script := UnboundedAgentInstallScript()
	require.NotEmpty(t, script)
	require.Contains(t, script, "#!/bin/bash")
	require.Contains(t, script, "unbounded-agent")
	require.Contains(t, script, `if [ -z "${AGENT_URL}" ]; then
    AGENT_URL="https://github.com/Azure/unbounded-kube/releases/download/${AGENT_VERSION}/unbounded-agent-linux-${arch}.tar.gz"
fi`)
	require.Contains(t, script, `curl -fsSL "${AGENT_URL}" | tar -xz -C /usr/local/bin unbounded-agent`)
}

func TestUnboundedAgentUninstallScript(t *testing.T) {
	t.Parallel()

	script := UnboundedAgentUninstallScript("my-test-node")
	require.NotEmpty(t, script)

	// Should be a valid bash script.
	require.Contains(t, script, "#!/bin/bash")
	require.Contains(t, script, "set -eo pipefail")

	// Machine name should be baked in, not the placeholder.
	require.Contains(t, script, `MACHINE_NAME="my-test-node"`)
	require.NotContains(t, script, "UNBOUNDED_MACHINE_NAME_PLACEHOLDER")

	// Should reference key cleanup operations.
	require.Contains(t, script, "machinectl stop")
	require.Contains(t, script, "machinectl terminate")
	require.Contains(t, script, "ip link delete")
	require.Contains(t, script, "/etc/systemd/nspawn/${MACHINE_NAME}.nspawn")
	require.Contains(t, script, "/var/lib/machines/${MACHINE_NAME}")
	require.Contains(t, script, "nftables-flush.service")
	require.Contains(t, script, "99-kubernetes.conf")
	require.Contains(t, script, "sysctl --system")
	require.Contains(t, script, "docker.service")
	require.Contains(t, script, "fstab.bak")
	require.Contains(t, script, "swapon")
	require.Contains(t, script, "unbounded-agent-uninstall.sh")
	require.Contains(t, script, "daemon-reload")
}

func TestUnboundedAgentUninstallScript_PlaceholderFullyReplaced(t *testing.T) {
	t.Parallel()

	script := UnboundedAgentUninstallScript("worker-42")

	// The placeholder should not appear anywhere in the rendered script.
	count := strings.Count(script, "UNBOUNDED_MACHINE_NAME_PLACEHOLDER")
	require.Equal(t, 0, count, "placeholder should be fully replaced")

	// The machine name should appear in multiple places (MACHINE_NAME var,
	// nspawn paths, rootfs paths, header comment).
	nameCount := strings.Count(script, "worker-42")
	require.GreaterOrEqual(t, nameCount, 2, "machine name should appear in multiple places")
}
