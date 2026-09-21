// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

	// Bootstrap preflight runs by default and can be explicitly disabled.
	require.Contains(t, script, "AGENT_PREFLIGHT:-true")
	require.Contains(t, script, "Running unbounded-agent preflight")
	require.Contains(t, script, "preflight ${_START_ARGS}")
	require.Contains(t, script, "0|false|no|FALSE|NO|False|No")

	// The installer must place the agent binary itself. The agent version is
	// selected independently of this script, including the default of tracking
	// the latest published release, so an installer that relies on the agent to
	// install its own binary breaks every agent released before that behavior
	// existed. The uninstall script removes this same path.
	require.Contains(t, script, `AGENT_BIN_TARGET="/usr/local/bin/unbounded-agent"`)
	require.Contains(t, script, `install -m 0755 "${AGENT_BIN}" "${AGENT_BIN_TARGET}"`)

	// It must not clobber a live binary. The test follows symlinks so a host
	// this installation already owns resolves through the compatibility symlink
	// to a live slot and is skipped, which keeps admission running from the
	// staged executable rather than one the retry just wrote.
	require.Contains(t, script, `if [ ! -x "${AGENT_BIN_TARGET}" ]; then`)
	require.Contains(t, script, `AGENT_BIN="${tmp_dir}/unbounded-agent"`)

	// The staged binary is executed, not just copied: admission runs from it.
	// The default temporary directory is therefore the wrong place for it,
	// because a host that mounts /tmp noexec cannot run it at all, and
	// image-based hosts are the ones most likely to be hardened that way.
	require.Contains(t, script, `mkdir -p "${staging_root}"`)
	require.Contains(t, script, `tmp_dir="$(mktemp -d "${staging_root}/install.XXXXXX")"`)
	require.NotContains(t, script, `tmp_dir="$(mktemp -d)"`)

	// Whatever is staged must still be cleaned up.
	require.Contains(t, script, `trap 'rm -rf "${tmp_dir}"' EXIT`)
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
