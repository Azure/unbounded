// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func mountOptions(t *testing.T, config []byte) map[string][]string {
	t.Helper()

	var s struct {
		Mounts []struct {
			Destination string
			Options     []string
		}
	}

	require.NoError(t, json.Unmarshal(config, &s))

	out := map[string][]string{}
	for _, m := range s.Mounts {
		out[m.Destination] = m.Options
	}

	return out
}

func hardeningInput() BundleInput {
	return BundleInput{
		Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1",
		SELinux: SELinuxPermissive,
		ExtraBinds: []ExtraBind{
			{Source: "/lib/modules", Target: "/lib/modules", ReadOnly: true},
			{Source: "/srv/rw", Target: "/srv/rw"},
		},
		Grants: []isolation.Grant{
			{Kind: isolation.GrantMount, Source: "/data/models", Target: "/models", ReadOnly: true},
			{Kind: isolation.GrantLibraryPath, Source: "/usr/lib/nvidia", Target: "/opt/nvidia/lib64"},
		},
	}
}

// A recursive bind with plain "ro" leaves the submounts writable; "rro" makes
// the whole tree read-only.
func TestReadOnlyBindsAreRecursivelyReadOnlyWhenTheRuntimeSupportsIt(t *testing.T) {
	in := hardeningInput()
	in.RecursiveReadOnly = true

	cfg, err := RenderConfig(in)
	require.NoError(t, err)

	opts := mountOptions(t, cfg)
	for _, dest := range []string{"/lib/modules", "/models", "/opt/nvidia/lib64"} {
		require.Equal(t, []string{"rbind", "rro"}, opts[dest], dest)
	}

	require.Equal(t, []string{"rbind", "rw"}, opts["/srv/rw"], "writable binds are unchanged")
}

func TestReadOnlyBindsStayRoWithoutRuntimeSupport(t *testing.T) {
	cfg, err := RenderConfig(hardeningInput())
	require.NoError(t, err)

	opts := mountOptions(t, cfg)
	for _, dest := range []string{"/lib/modules", "/models", "/opt/nvidia/lib64"} {
		require.Equal(t, []string{"rbind", "ro"}, opts[dest], dest)
	}
}

func TestTargetsAreCleanedBeforeTheConflictCheck(t *testing.T) {
	in := BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, ExtraBinds: []ExtraBind{
		{Source: "/x", Target: "/run/containerd/../containerd"},
	}}

	_, err := RenderConfig(in)
	require.ErrorContains(t, err, "conflicts")

	in = BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, Grants: []isolation.Grant{
		{Kind: isolation.GrantMount, Source: "/data", Target: "/etc/containerd/certs.d/"},
	}}

	_, err = RenderConfig(in)
	require.ErrorContains(t, err, "conflicts")

	in = BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, Grants: []isolation.Grant{
		{Kind: isolation.GrantMount, Source: "/data//models/", Target: "/mnt/./models/"},
	}}

	cfg, err := RenderConfig(in)
	require.NoError(t, err)

	opts := mountOptions(t, cfg)
	require.Contains(t, opts, "/mnt/models", "the cleaned target is what is mounted")
	require.NotContains(t, opts, "/mnt/./models/")
}

func TestTargetsUnderProcAndSysAreRefused(t *testing.T) {
	for _, target := range []string{"/proc", "/proc/sys", "/sys", "/sys/kernel/security", "/proc/../proc/sys", "/sys/fs/cgroup/x"} {
		_, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, ExtraBinds: []ExtraBind{{Source: "/x", Target: target}}})
		require.ErrorContains(t, err, "/proc or /sys", "extra bind "+target)

		_, err = RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, Grants: []isolation.Grant{{Kind: isolation.GrantMount, Source: "/x", Target: target}}})
		require.ErrorContains(t, err, "/proc or /sys", "grant "+target)
	}

	// Only the subtrees; a sibling that shares the prefix text is fine.
	_, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive, ExtraBinds: []ExtraBind{{Source: "/x", Target: "/procfs-data"}, {Source: "/y", Target: "/system"}}})
	require.NoError(t, err)

	// The fixed same-path mount of /sys/fs/bpf is not user input.
	cfg, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: SELinuxPermissive})
	require.NoError(t, err)
	require.Contains(t, mountOptions(t, cfg), "/sys/fs/bpf")
}

func TestRecursiveReadOnlySupportNeedsBothTheRuntimeAndTheKernel(t *testing.T) {
	for _, tc := range []struct {
		runc, kernel string
		want         bool
	}{
		{"runc version 1.1.12\ncommit: x\nspec: 1.0.2-dev\n", "6.8.0-137-generic", true},
		{"runc version 1.2.0\n", "5.12.0", true},
		{"runc version 1.1.0\n", "5.15.0-91-generic", true},
		{"runc version 2.0.0\n", "6.1.0", true},
		{"runc version 1.0.3\n", "6.8.0", false},
		{"runc version 1.2.0\n", "5.11.9", false},
		{"runc version 1.2.0\n", "4.18.0-553.el8.x86_64", false},
		{"runc version 1.2.0\n", "", false},
		{"runc version 1.2.0\n", "garbage", false},
		{"", "6.8.0", false},
		{"runc version dev\n", "6.8.0", false},
	} {
		require.Equal(t, tc.want, supportsRecursiveReadOnly(tc.runc, tc.kernel), "runc %q kernel %q", tc.runc, tc.kernel)
	}
}

func TestRenderBundleUsesRroOnlyWhenTheInstalledRuntimeAndKernelAllow(t *testing.T) {
	grants := []isolation.Grant{{Kind: isolation.GrantMount, Source: "/data/models", Target: "/models", ReadOnly: true}}

	render := func(t *testing.T, version string, versionErr error, kernel string) []string {
		t.Helper()

		b, st, _ := newBackend(t)
		st.out[rt+" --version"] = version

		if versionErr != nil {
			st.fail[rt+" --version"] = versionErr
		}

		b.KernelRelease = func() string { return kernel }

		require.NoError(t, b.RenderBundle("kube1", grants))

		data, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
		require.NoError(t, err)

		return mountOptions(t, data)["/models"]
	}

	require.Equal(t, []string{"rbind", "rro"}, render(t, "runc version 1.1.12\n", nil, "6.8.0-137-generic"))
	require.Equal(t, []string{"rbind", "ro"}, render(t, "runc version 1.0.3\n", nil, "6.8.0-137-generic"), "an old runc keeps ro")
	require.Equal(t, []string{"rbind", "ro"}, render(t, "runc version 1.2.0\n", nil, "5.10.0"), "an old kernel keeps ro")
	require.Equal(t, []string{"rbind", "ro"}, render(t, "", errors.New("not installed yet"), "6.8.0"), "no runtime yet keeps ro")
	require.Equal(t, []string{"rbind", "ro"}, render(t, "unparsable", nil, "6.8.0"))
}

func TestRenderBundleWithoutKernelInfoKeepsRo(t *testing.T) {
	b, st, _ := newBackend(t)
	st.out[rt+" --version"] = "runc version 1.2.0\n"

	require.NoError(t, b.RenderBundle("kube1", []isolation.Grant{{Kind: isolation.GrantMount, Source: "/d", Target: "/d", ReadOnly: true}}))

	data, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Equal(t, []string{"rbind", "ro"}, mountOptions(t, data)["/d"], "the test host has no /proc/sys/kernel/osrelease under SourceRoot")
}
