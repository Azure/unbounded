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

	// The install script must support the documented download-override
	// environment variables.
	require.Contains(t, script, "AGENT_VERSION")
	require.Contains(t, script, "AGENT_BASE_URL")
	require.Contains(t, script, "AGENT_URL")

	// The default base URL must point at GitHub releases so that a fresh
	// install works out of the box.
	require.Contains(t, script, "https://github.com/Azure/unbounded/releases")

	// The default (unpinned) download URL must use GitHub's /latest/download/
	// redirect so a new release is picked up without editing this script.
	require.Contains(t, script, "/latest/download/unbounded-agent-linux-")

	// Pinned downloads must use the /download/<tag>/ layout.
	require.Contains(t, script, "/download/${AGENT_VERSION}/unbounded-agent-linux-")

	// The script must not hardcode a specific release tag as a fallback
	// default, so that "latest" is used when AGENT_VERSION is unset.
	require.NotContains(t, script, "AGENT_VERSION:-v0.0.10")
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
