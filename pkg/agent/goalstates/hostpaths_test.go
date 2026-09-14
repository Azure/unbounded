// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
)

func TestHostPrefixOrDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultHostPrefix, HostPrefixOrDefault(""))
	assert.Equal(t, DefaultHostPrefix, HostPrefixOrDefault("   "))
	assert.Equal(t, "/opt/unbounded", HostPrefixOrDefault("/opt/unbounded"))
	assert.Equal(t, "/opt/unbounded", HostPrefixOrDefault("  /opt/unbounded  "))
}

// TestResolveHostPathsDefaultsAreUnchanged pins the pre-existing absolute paths.
// Hosts that do not configure a prefix must keep exactly the layout they had
// before the prefix became configurable.
func TestResolveHostPathsDefaultsAreUnchanged(t *testing.T) {
	t.Parallel()

	paths := ResolveHostPaths("")

	assert.Equal(t, "/usr/local", paths.Prefix)
	assert.Equal(t, "/usr/local/bin", paths.BinDir)
	assert.Equal(t, "/usr/local/libexec", paths.LibexecDir)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-nspawn-lifecycle", paths.NSpawnLifecycleBinary)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-daemon-recovery.sh", paths.DaemonRecoveryScript)
	assert.Equal(t, "/usr/local/libexec/unbounded-localdns-network", paths.LocalDNSNetworkHelper)
}

func TestResolveHostPathsWithPrefix(t *testing.T) {
	t.Parallel()

	paths := ResolveHostPaths("/opt/unbounded")

	assert.Equal(t, "/opt/unbounded", paths.Prefix)
	assert.Equal(t, "/opt/unbounded/bin", paths.BinDir)
	assert.Equal(t, "/opt/unbounded/libexec", paths.LibexecDir)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-nspawn-lifecycle", paths.NSpawnLifecycleBinary)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-daemon-recovery.sh", paths.DaemonRecoveryScript)
	assert.Equal(t, "/opt/unbounded/libexec/unbounded-localdns-network", paths.LocalDNSNetworkHelper)
}

// TestResolvedAgentUpgradePathsDefaultsAreUnchanged is the equivalent regression
// guard for the blue-green daemon binary layout.
func TestResolvedAgentUpgradePathsDefaultsAreUnchanged(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "")
	t.Setenv(EnvDaemonBinaryBlue, "")
	t.Setenv(EnvDaemonBinaryGreen, "")
	t.Setenv(EnvDaemonBinaryCurrent, "")
	t.Setenv(EnvDaemonBinaryLastGood, "")

	paths, err := ResolvedAgentUpgradePaths("")
	require.NoError(t, err)

	assert.Equal(t, "/usr/local/bin/unbounded-agent", paths.BinaryPath)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-blue", paths.BluePath)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-green", paths.GreenPath)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-current", paths.CurrentPath)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-last-good", paths.LastGoodPath)
	assert.Equal(t, "/usr/local/bin/unbounded-agent-daemon-recovery.sh", paths.RecoveryScriptPath)
}

func TestResolvedAgentUpgradePathsWithPrefix(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "")
	t.Setenv(EnvDaemonBinaryBlue, "")
	t.Setenv(EnvDaemonBinaryGreen, "")
	t.Setenv(EnvDaemonBinaryCurrent, "")
	t.Setenv(EnvDaemonBinaryLastGood, "")

	paths, err := ResolvedAgentUpgradePaths("/opt/unbounded")
	require.NoError(t, err)

	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent", paths.BinaryPath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-current", paths.CurrentPath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-daemon-recovery.sh", paths.RecoveryScriptPath)
}

// TestResolvedAgentUpgradePathsEnvOverridesPrefix keeps the existing escape
// hatch working: an explicit environment override wins over the prefix.
func TestResolvedAgentUpgradePathsEnvOverridesPrefix(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "/somewhere/else/unbounded-agent")

	paths, err := ResolvedAgentUpgradePaths("/opt/unbounded")
	require.NoError(t, err)

	assert.Equal(t, "/somewhere/else/unbounded-agent", paths.BinaryPath)
	// Paths without an override still follow the prefix.
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-blue", paths.BluePath)
}

func TestKnownHostPrefixes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{DefaultHostPrefix}, KnownHostPrefixes(""))
	assert.Equal(t, []string{DefaultHostPrefix}, KnownHostPrefixes(DefaultHostPrefix))

	// A non-default prefix must still sweep the default, so that a host
	// provisioned under the old layout is not left with orphaned files.
	assert.Equal(t, []string{"/opt/unbounded", DefaultHostPrefix}, KnownHostPrefixes("/opt/unbounded"))
}

func TestHostPrefixFromAppliedConfig(t *testing.T) {
	// AppliedConfigPath is absolute, so redirect it by pointing AgentConfigDir's
	// consumers at a temporary root is not possible; instead assert the
	// fallback, which is the branch reachable without writing to /etc.
	assert.Equal(t, DefaultHostPrefix, HostPrefixFromAppliedConfig())
}

// TestHostPrefixRoundTripsThroughAppliedConfig proves the persisted config
// carries the prefix, which is what lets systemd-started processes resolve the
// same paths the bootstrap used.
func TestHostPrefixRoundTripsThroughAppliedConfig(t *testing.T) {
	t.Parallel()

	cfg := config.AgentConfig{MachineName: "m", HostPrefix: "/opt/unbounded"}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "applied-config.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var decoded config.AgentConfig
	require.NoError(t, json.Unmarshal(raw, &decoded))

	assert.Equal(t, "/opt/unbounded", decoded.HostPrefix)
	assert.Equal(t, "/opt/unbounded/bin", ResolveHostPaths(decoded.HostPrefix).BinDir)
}
