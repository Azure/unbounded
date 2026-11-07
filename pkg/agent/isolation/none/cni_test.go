// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func cniBackend(t *testing.T) *Backend {
	t.Helper()

	b, _ := newBackend(t)
	b.HostRoot = t.TempDir()

	return b
}

func writePlugin(t *testing.T, b *Backend, version, name string) {
	t.Helper()

	dir := filepath.Join(b.Layout.VersionDir(version), cniBinSubdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(version), 0o755))
}

// The plugins live in the version and the host directory links each through
// the current link, so a flip switches them with the rest of the version.
func TestLinkCNIPluginsFollowTheCurrentLink(t *testing.T) {
	b := cniBackend(t)
	writePlugin(t, b, "v1", "bridge")
	writePlugin(t, b, "v2", "bridge")

	require.NoError(t, b.LinkCNIPlugins("v1"))
	_, err := b.Layout.Flip("v1")
	require.NoError(t, err)

	host := filepath.Join(b.hostCNIBinDir(), "bridge")
	data, err := os.ReadFile(host)
	require.NoError(t, err)
	require.Equal(t, "v1", string(data))

	require.NoError(t, b.LinkCNIPlugins("v2"), "idempotent")
	_, err = b.Layout.Flip("v2")
	require.NoError(t, err)

	data, err = os.ReadFile(host)
	require.NoError(t, err)
	require.Equal(t, "v2", string(data))
}

// A plugin another component installed (unbounded-net's IPAM plugin, say) is
// left alone, whether it is a file or a link of its own.
func TestLinkCNIPluginsLeavesOtherPluginsAlone(t *testing.T) {
	b := cniBackend(t)
	writePlugin(t, b, "v1", "host-local")
	writePlugin(t, b, "v1", "portmap")

	dir := b.hostCNIBinDir()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host-local"), []byte("theirs"), 0o755))
	require.NoError(t, os.Symlink("/usr/lib/cni/portmap", filepath.Join(dir, "portmap")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unbounded-ipam"), []byte("ipam"), 0o755))

	require.NoError(t, b.LinkCNIPlugins("v1"))

	data, err := os.ReadFile(filepath.Join(dir, "host-local"))
	require.NoError(t, err)
	require.Equal(t, "theirs", string(data))

	target, err := os.Readlink(filepath.Join(dir, "portmap"))
	require.NoError(t, err)
	require.Equal(t, "/usr/lib/cni/portmap", target)

	// Destroy removes only the agent's links.
	require.NoError(t, b.UnlinkCNIPlugins())
	require.FileExists(t, filepath.Join(dir, "host-local"))
	require.FileExists(t, filepath.Join(dir, "unbounded-ipam"))

	_, err = os.Lstat(filepath.Join(dir, "portmap"))
	require.NoError(t, err)
}

func TestUnlinkCNIPluginsRemovesTheDirectoryWhenItIsLeftEmpty(t *testing.T) {
	b := cniBackend(t)
	writePlugin(t, b, "v1", "bridge")
	require.NoError(t, b.LinkCNIPlugins("v1"))

	require.NoError(t, b.UnlinkCNIPlugins())
	require.NoDirExists(t, b.hostCNIBinDir())
	require.NoError(t, b.UnlinkCNIPlugins(), "nothing to do is not an error")
}

// Prepare links the plugins it installed and Destroy unlinks them.
func TestPrepareAndDestroyManageTheCNIPluginLinks(t *testing.T) {
	b := cniBackend(t)
	b.Install = func(_ context.Context, r isolation.NodeRoot) error {
		dir := r.Path("/opt/cni/bin")
		require.NoError(t, os.MkdirAll(dir, 0o755))

		return os.WriteFile(filepath.Join(dir, "loopback"), nil, 0o755)
	}

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	link := filepath.Join(b.hostCNIBinDir(), "loopback")
	target, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(b.Layout.CNIBinDir(), "loopback"), target)

	require.NoError(t, b.Destroy(context.Background(), root("v1")))

	_, err = os.Lstat(link)
	require.ErrorIs(t, err, os.ErrNotExist)
}
