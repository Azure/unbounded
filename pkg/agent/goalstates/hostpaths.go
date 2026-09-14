// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// DefaultHostPrefix is the installation prefix used when the agent config does
// not set one.
const DefaultHostPrefix = "/usr/local"

// Base names of the agent's own host-side files. They are joined with the
// resolved prefix rather than being absolute constants so that hosts with a
// read-only /usr can place them somewhere writable.
const (
	daemonBinaryName          = "unbounded-agent"
	daemonBinaryBlueName      = "unbounded-agent-blue"
	daemonBinaryGreenName     = "unbounded-agent-green"
	daemonBinaryCurrentName   = "unbounded-agent-current"
	daemonBinaryLastGoodName  = "unbounded-agent-last-good"
	nspawnLifecycleName       = "unbounded-agent-nspawn-lifecycle"
	daemonRecoveryScriptName  = "unbounded-agent-daemon-recovery.sh"
	localDNSNetworkHelperName = "unbounded-localdns-network"
)

// HostPaths is the resolved host-side layout of the agent's own files under an
// installation prefix.
//
// These are paths on the host. Files inside the nspawn machine are always
// resolved relative to the machine directory and are unaffected by the prefix.
type HostPaths struct {
	// Prefix is the resolved installation prefix.
	Prefix string
	// BinDir is <Prefix>/bin.
	BinDir string
	// LibexecDir is <Prefix>/libexec.
	LibexecDir string

	// NSpawnLifecycleBinary is the rollback-stable helper invoked by the
	// generated nspawn hook units.
	NSpawnLifecycleBinary string
	// DaemonRecoveryScript is executed by the daemon recovery unit.
	DaemonRecoveryScript string
	// LocalDNSNetworkHelper backs unbounded-localdns-network.service.
	LocalDNSNetworkHelper string
}

// HostPrefixOrDefault returns the configured prefix, or DefaultHostPrefix when
// it is empty.
func HostPrefixOrDefault(prefix string) string {
	if trimmed := strings.TrimSpace(prefix); trimmed != "" {
		return trimmed
	}

	return DefaultHostPrefix
}

// ResolveHostPaths returns the host-side agent layout for an installation
// prefix. An empty prefix selects DefaultHostPrefix.
func ResolveHostPaths(prefix string) HostPaths {
	resolved := HostPrefixOrDefault(prefix)
	binDir := filepath.Join(resolved, "bin")
	libexecDir := filepath.Join(resolved, "libexec")

	return HostPaths{
		Prefix:                resolved,
		BinDir:                binDir,
		LibexecDir:            libexecDir,
		NSpawnLifecycleBinary: filepath.Join(binDir, nspawnLifecycleName),
		DaemonRecoveryScript:  filepath.Join(binDir, daemonRecoveryScriptName),
		LocalDNSNetworkHelper: filepath.Join(libexecDir, localDNSNetworkHelperName),
	}
}

// KnownHostPrefixes returns the prefixes that teardown and existing-deployment
// detection must consider.
//
// A host provisioned before the prefix was configurable, or by an agent using a
// different prefix, still has files under the default. Cleanup and
// already-provisioned checks therefore look at both, so that changing the
// prefix cannot orphan files or let a dirty host be silently reprovisioned.
func KnownHostPrefixes(prefix string) []string {
	resolved := HostPrefixOrDefault(prefix)
	if resolved == DefaultHostPrefix {
		return []string{DefaultHostPrefix}
	}

	return []string{resolved, DefaultHostPrefix}
}

// HostPrefixFromAppliedConfig returns the installation prefix recorded in the
// applied config of whichever machine is provisioned on this host.
//
// Processes started by systemd, such as the agent daemon and the nspawn
// lifecycle hooks, cannot inherit the prefix from the environment that
// bootstrapped the host. The applied config is the authoritative record: it is
// written once at bootstrap and re-read here so that later upgrades and
// teardown resolve the same paths the bootstrap used.
//
// An absent or unreadable config yields the default prefix, which is what a
// host provisioned before the prefix was configurable actually has on disk.
func HostPrefixFromAppliedConfig() string {
	for _, name := range []string{NSpawnMachineKube1, NSpawnMachineKube2} {
		data, err := os.ReadFile(AppliedConfigPath(name))
		if err != nil {
			continue
		}

		// Only the prefix is needed here, so decode into the shared config type
		// rather than a consumer-specific wrapper. Unknown fields are ignored.
		var cfg config.AgentConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}

		if prefix := HostPrefixOrDefault(cfg.HostPrefix); prefix != DefaultHostPrefix {
			return prefix
		}
	}

	return DefaultHostPrefix
}
