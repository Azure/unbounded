// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"encoding/json"
	"errors"
	"log/slog"
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

// MergeHostPrefixes returns every distinct prefix teardown must sweep, given
// candidates gathered from different sources.
//
// Teardown cannot rely on any single source. The installation record has the
// prefix from before the first mutation but may be absent on hosts provisioned
// by an older agent; the applied config has it only once the node started. An
// empty candidate contributes nothing but never suppresses the default.
func MergeHostPrefixes(candidates ...string) []string {
	var (
		out  []string
		seen = map[string]struct{}{}
	)

	add := func(prefix string) {
		if _, ok := seen[prefix]; ok {
			return
		}

		seen[prefix] = struct{}{}

		out = append(out, prefix)
	}

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}

		for _, prefix := range KnownHostPrefixes(candidate) {
			add(prefix)
		}
	}

	add(DefaultHostPrefix)

	return out
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
//
// The applied config only exists once the node has started, so this returns the
// default on a host where bootstrap failed before then. Callers that must be
// right in that case should ask the installation record first, which carries the
// same prefix and is written before the first host mutation.
func HostPrefixFromAppliedConfig(log *slog.Logger) string {
	return hostPrefixFromAppliedConfigIn(log, AgentConfigDir)
}

// hostPrefixFromAppliedConfigIn takes the config directory so the lookup can be
// exercised without reading the real /etc. Without this the only reachable
// branch in a test is the fallback, and on a provisioned host even that answer
// depends on what happens to be installed.
func hostPrefixFromAppliedConfigIn(log *slog.Logger, configDir string) string {
	for _, name := range []string{NSpawnMachineKube1, NSpawnMachineKube2} {
		path := appliedConfigPathIn(configDir, name)

		data, err := os.ReadFile(path)
		if err != nil {
			// A machine that was never provisioned has no applied config, which
			// is ordinary. Anything else is worth saying out loud, because the
			// fallback is the one prefix known to be unwritable on a host that
			// configured one.
			if log != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("cannot read applied config while resolving the host prefix", "path", path, "error", err)
			}

			continue
		}

		// Only the prefix is needed here, so decode into the shared config type
		// rather than a consumer-specific wrapper. Unknown fields are ignored.
		var cfg config.AgentConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			if log != nil {
				log.Warn("applied config is unreadable while resolving the host prefix", "path", path, "error", err)
			}

			continue
		}

		if prefix := HostPrefixOrDefault(cfg.HostPrefix); prefix != DefaultHostPrefix {
			return prefix
		}
	}

	return DefaultHostPrefix
}

// Base names of the legacy installer scripts. They are not installed by the
// agent any more, but hosts provisioned by older versions still carry them and
// teardown has to remove them.
const (
	agentInstallScriptName   = "unbounded-agent-install.sh"
	agentUninstallScriptName = "unbounded-agent-uninstall.sh"
)

// OwnedHostFiles returns every file the agent installs under a single prefix.
//
// Teardown and the existing-deployment preflight both need this list, and they
// have to agree: a file teardown does not remove is one preflight will later
// refuse to provision over, and a file preflight does not look for is one that
// can be silently provisioned on top of. Defining it once is what keeps those
// two from drifting.
//
// Environment overrides are deliberately not applied. These are the paths the
// agent installs to as a matter of layout, and teardown needs to find them on a
// host whose environment no longer resembles the one that provisioned it.
func OwnedHostFiles(prefix string) []string {
	paths := ResolveHostPaths(prefix)

	return []string{
		filepath.Join(paths.BinDir, daemonBinaryName),
		filepath.Join(paths.BinDir, daemonBinaryBlueName),
		filepath.Join(paths.BinDir, daemonBinaryGreenName),
		filepath.Join(paths.BinDir, daemonBinaryCurrentName),
		filepath.Join(paths.BinDir, daemonBinaryLastGoodName),
		paths.NSpawnLifecycleBinary,
		paths.DaemonRecoveryScript,
		paths.LocalDNSNetworkHelper,
		filepath.Join(paths.BinDir, agentInstallScriptName),
		filepath.Join(paths.BinDir, agentUninstallScriptName),
	}
}

// OwnedHostFilesAcross returns the agent's files under every prefix the host
// might hold them under, for callers that must not miss a layout left behind by
// an earlier prefix.
func OwnedHostFilesAcross(candidates ...string) []string {
	var out []string
	for _, prefix := range MergeHostPrefixes(candidates...) {
		out = append(out, OwnedHostFiles(prefix)...)
	}

	return out
}
