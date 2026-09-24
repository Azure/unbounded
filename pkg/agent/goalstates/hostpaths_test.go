// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestKnownHostPrefixes covers the sweep list teardown and existing-deployment
// detection work from.
//
// A non-default prefix must still yield the default, or a host provisioned
// under the old layout and then reconfigured would have the old files left
// behind with nothing looking for them.
func TestKnownHostPrefixes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{DefaultHostPrefix}, KnownHostPrefixes(""))
	assert.Equal(t, []string{DefaultHostPrefix}, KnownHostPrefixes(DefaultHostPrefix))

	// A non-default prefix must still sweep the default, so that a host
	// provisioned under the old layout is not left with orphaned files.
	assert.Equal(t, []string{"/opt/unbounded", DefaultHostPrefix}, KnownHostPrefixes("/opt/unbounded"))
}

func TestHostPrefixFromAppliedConfig(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, dir, machine, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(appliedConfigPathIn(dir, machine), []byte(body), 0o600))
	}

	prefixed := `{"MachineName":"m","HostPrefix":"/opt/unbounded"}`

	t.Run("prefix in the first slot", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube1, prefixed)

		assert.Equal(t, "/opt/unbounded", hostPrefixFromAppliedConfigIn(nil, dir))
	})

	// After an ordinary repave the live machine is the second slot, so a lookup
	// that only ever read the first would resolve the default on a host that
	// has none of its files there.
	t.Run("prefix only in the second slot", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube2, prefixed)

		assert.Equal(t, "/opt/unbounded", hostPrefixFromAppliedConfigIn(nil, dir))
	})

	t.Run("no applied config yields the default", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, DefaultHostPrefix, hostPrefixFromAppliedConfigIn(nil, t.TempDir()))
	})

	// A corrupt config must not stop the other slot from answering. Returning
	// the default here would send every later caller at /usr/local, which is
	// the one directory known unwritable on a host that configured a prefix.
	t.Run("corrupt config does not mask the other slot", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube1, "{not json")
		write(t, dir, NSpawnMachineKube2, prefixed)

		assert.Equal(t, "/opt/unbounded", hostPrefixFromAppliedConfigIn(nil, dir))
	})

	t.Run("config matching its checksum is used", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube1, prefixed)
		require.NoError(t, os.WriteFile(appliedConfigChecksumPathIn(dir, NSpawnMachineKube1),
			[]byte(ComputeChecksum([]byte(prefixed))+"\n"), 0o600))

		assert.Equal(t, "/opt/unbounded", hostPrefixFromAppliedConfigIn(nil, dir))
	})

	// FindActiveMachine refuses a config that fails its checksum, and so must
	// this: the prefix picks which directories are written to and swept.
	t.Run("config failing its checksum is skipped", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube1, `{"MachineName":"m","HostPrefix":"/opt/corrupt"}`)
		require.NoError(t, os.WriteFile(appliedConfigChecksumPathIn(dir, NSpawnMachineKube1),
			[]byte(ComputeChecksum([]byte(prefixed))+"\n"), 0o600))
		write(t, dir, NSpawnMachineKube2, prefixed)

		assert.Equal(t, "/opt/unbounded", hostPrefixFromAppliedConfigIn(nil, dir))
	})

	t.Run("config without a prefix yields the default", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		write(t, dir, NSpawnMachineKube1, `{"MachineName":"m"}`)

		assert.Equal(t, DefaultHostPrefix, hostPrefixFromAppliedConfigIn(nil, dir))
	})
}

// TestMergeHostPrefixesOrdering pins the sweep order, which is not obvious.
//
// KnownHostPrefixes appends the default per candidate, so with more than one
// candidate the default lands in the middle rather than at the end. Teardown
// reads this list, and anything that stops early or treats position as meaning
// would be affected, so the order is fixed here rather than discovered later.
func TestMergeHostPrefixesOrdering(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{DefaultHostPrefix}, MergeHostPrefixes())
	assert.Equal(t, []string{DefaultHostPrefix}, MergeHostPrefixes("", "  "))
	assert.Equal(t, []string{"/opt/a", DefaultHostPrefix}, MergeHostPrefixes("/opt/a"))
	assert.Equal(t, []string{"/opt/a", DefaultHostPrefix}, MergeHostPrefixes("/opt/a", "/opt/a"))
	assert.Equal(t, []string{"/opt/a", DefaultHostPrefix, "/opt/b"}, MergeHostPrefixes("/opt/a", "/opt/b"))

	// Every candidate has to survive, or teardown sweeps somewhere the files
	// are not. Duplicates must not, or it sweeps the same place twice.
	merged := MergeHostPrefixes("/opt/a", "", DefaultHostPrefix, "/opt/b")
	assert.ElementsMatch(t, []string{"/opt/a", "/opt/b", DefaultHostPrefix}, merged)
	assert.Len(t, merged, 3)
}

// TestOwnedHostFilesFollowThePrefix pins the layout teardown removes.
func TestOwnedHostFilesFollowThePrefix(t *testing.T) {
	t.Parallel()

	files := OwnedHostFiles("/opt/unbounded")
	require.NotEmpty(t, files)

	for _, path := range files {
		assert.True(t, strings.HasPrefix(path, "/opt/unbounded/"),
			"%s must sit under the configured prefix", path)
	}

	// The helper that is not in bin/ has to move with the prefix too, or
	// teardown leaves it behind on exactly the hosts that configure one.
	assert.Contains(t, files, "/opt/unbounded/libexec/unbounded-localdns-network")
	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent")
	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent-daemon-recovery.sh")
	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent-nspawn-lifecycle")

	// Legacy installer scripts are no longer written but still exist on hosts
	// provisioned by older agents, so teardown must still name them.
	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent-install.sh")
	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent-uninstall.sh")
}

// TestOwnedHostFilesAcrossCoversTheAbandonedLayout is the reprovisioning case.
//
// A host that was installed under one prefix and reprovisioned under another
// still has the first layout on disk. Teardown that swept only the current
// prefix would orphan those files.
func TestOwnedHostFilesAcrossCoversTheAbandonedLayout(t *testing.T) {
	t.Parallel()

	files := OwnedHostFilesAcross("/opt/unbounded")

	assert.Contains(t, files, "/opt/unbounded/bin/unbounded-agent")
	assert.Contains(t, files, "/usr/local/bin/unbounded-agent")

	// No prefix at all still sweeps the default, and only the default.
	for _, path := range OwnedHostFilesAcross("") {
		assert.True(t, strings.HasPrefix(path, DefaultHostPrefix+"/"), path)
	}

	// Every path is distinct: sweeping the same file twice is harmless but
	// signals the prefix merge stopped deduplicating.
	seen := map[string]struct{}{}
	for _, path := range files {
		_, dup := seen[path]
		assert.False(t, dup, "duplicate path %s", path)
		seen[path] = struct{}{}
	}
}
