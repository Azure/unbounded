// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveAgentUpgrade_UsesBlueWhenCurrentIsNotBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:     "/agent-blue",
		GreenPath:    "/agent-green",
		CurrentPath:  "/agent-current",
		LastGoodPath: "/agent-last-good",
	}

	upgrade := paths.ResolveAgentUpgrade("https://example.com/agent.tar.gz", "/agent")

	assert.Equal(t, "https://example.com/agent.tar.gz", upgrade.DownloadURL)
	assert.Equal(t, AgentUpgradeBinaryName, upgrade.BinaryName)
	assert.Equal(t, "/agent", upgrade.PreviousBinaryPath)
	assert.Equal(t, "/agent-blue", upgrade.TargetBinaryPath)
	assert.Equal(t, "/agent-current", upgrade.CurrentLinkPath)
	assert.Equal(t, "/agent-last-good", upgrade.LastGoodLinkPath)
	assert.Equal(t, 0o755, int(upgrade.BinaryMode))
}

func TestResolveAgentUpgrade_UsesGreenWhenCurrentIsBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:     "/agent-blue",
		GreenPath:    "/agent-green",
		CurrentPath:  "/agent-current",
		LastGoodPath: "/agent-last-good",
	}

	upgrade := paths.ResolveAgentUpgrade("https://example.com/agent.tar.gz", "/agent-blue")

	assert.Equal(t, "/agent-green", upgrade.TargetBinaryPath)
	assert.Equal(t, "/agent-blue", upgrade.PreviousBinaryPath)
}
