// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGoalStateSourcePrefersTheGivenGoalState(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{}
	resolved := false

	got, err := GoalStateSource{
		GoalState: gs,
		Resolve:   func(string) (*goalstates.MachineGoalState, error) { resolved = true; return nil, nil },
	}.For("kube1")
	require.NoError(t, err)
	assert.Same(t, gs, got)
	assert.False(t, resolved, "a goal state the caller already has is not resolved again")
}

func TestGoalStateSourceResolvesTheSlotAndWrapsFailures(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{}

	var slot string

	got, err := GoalStateSource{Resolve: func(s string) (*goalstates.MachineGoalState, error) { slot = s; return gs, nil }}.For("kube2")
	require.NoError(t, err)
	assert.Same(t, gs, got)
	assert.Equal(t, "kube2", slot)

	boom := errors.New("boom")
	_, err = GoalStateSource{Resolve: func(string) (*goalstates.MachineGoalState, error) { return nil, boom }}.For("kube1")
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "resolve machine goal state")
}

func TestGoalStateSourceWithoutConfigurationFails(t *testing.T) {
	t.Parallel()

	_, err := GoalStateSource{Log: quiet()}.For("kube1")
	require.ErrorContains(t, err, "no agent configuration")
}

func TestNetworkStepsUseTheirOverride(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	var ran []string

	mark := func(name string, err error) func(context.Context) error {
		return func(context.Context) error { ran = append(ran, name); return err }
	}

	h := Hooks{SetupNetwork: mark("setup", nil), CleanupNetwork: mark("cleanup", nil), WaitLocalDNS: mark("wait", boom)}

	require.NoError(t, h.SetupLocalDNSNetwork(t.Context(), quiet(), nil))
	require.NoError(t, h.CleanupHostNetwork(t.Context(), quiet()))
	require.ErrorIs(t, h.AwaitLocalDNS(t.Context(), quiet(), nil), boom)
	assert.Equal(t, []string{"setup", "cleanup", "wait"}, ran)
}

type statusBackend struct {
	isolation.FakeBackend
	status isolation.Status
	err    error
}

func (b *statusBackend) Status(context.Context, isolation.SlotRef) (isolation.Status, error) {
	return b.status, b.err
}

func TestRunningReportsTheBackendStatus(t *testing.T) {
	t.Parallel()

	root := isolation.DirRoot{SlotName: "kube1"}

	up, err := Running(t.Context(), &statusBackend{status: isolation.Status{Running: true}}, root)
	require.NoError(t, err)
	assert.True(t, up)

	up, err = Running(t.Context(), &statusBackend{}, root)
	require.NoError(t, err)
	assert.False(t, up)

	boom := errors.New("boom")
	up, err = Running(t.Context(), &statusBackend{status: isolation.Status{Running: true}, err: boom}, root)
	require.ErrorIs(t, err, boom)
	assert.False(t, up, "a status error is never reported as running")
}

func TestRestartChanged(t *testing.T) {
	t.Parallel()

	units := []ChangedUnit{{Unit: "containerd", Changed: true}, {Unit: "kubelet", Changed: false}, {Unit: "dns", Changed: true}}

	t.Run("a node that was not running restarts nothing", func(t *testing.T) {
		t.Parallel()

		err := RestartChanged(t.Context(), quiet(), false, units, func(context.Context, string) error {
			t.Fatal("a start already read the configuration")
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("only the changed units restart, in order", func(t *testing.T) {
		t.Parallel()

		var got []string

		err := RestartChanged(t.Context(), quiet(), true, units, func(_ context.Context, u string) error {
			got = append(got, u)
			return nil
		}, "slot", "kube1")
		require.NoError(t, err)
		assert.Equal(t, []string{"containerd", "dns"}, got)
	})

	t.Run("the first failure stops the rest and is returned as is", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("boom")

		var got []string

		err := RestartChanged(t.Context(), quiet(), true, units, func(_ context.Context, u string) error {
			got = append(got, u)
			return boom
		})
		require.ErrorIs(t, err, boom)
		assert.Equal(t, []string{"containerd"}, got)
	})
}

func TestSnapshotDir(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/x/y", Hooks{ConfigSnapshotDir: "/x/y", HostRoot: "/root"}.SnapshotDir("name"))
	assert.Equal(t, filepath.Join("/root", goalstates.AgentConfigDir, "name"), Hooks{HostRoot: "/root"}.SnapshotDir("name"))
}

func TestSnapshotRestoresTheSharedConfigurationAndDiscards(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, goalstates.KubeletConfigurationPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, os.WriteFile(file, []byte("old"), 0o600))

	dir := filepath.Join(root, "snapshot")

	s, err := TakeSnapshot(quiet(), dir, root)
	require.NoError(t, err)
	assert.False(t, s.Restored())

	require.NoError(t, os.WriteFile(file, []byte("new"), 0o600))
	require.NoError(t, s.Restore())
	assert.True(t, s.Restored())

	data, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "old", string(data))

	assert.DirExists(t, dir, "restoring keeps the copy until it is discarded")
	s.Discard()
	assert.NoDirExists(t, dir)
}

// Restoring drop-ins changes unit definitions systemd has loaded, so the
// previous version would start on the new ones until a reload.
func TestSnapshotRestoreReloadsSystemdAfterTheFilesAreBack(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, goalstates.KubeletConfigurationPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, os.WriteFile(file, []byte("old"), 0o600))

	s, err := TakeSnapshot(quiet(), filepath.Join(root, "snapshot"), root)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(file, []byte("new"), 0o600))

	var sawRestored string

	s.reload = func() error {
		data, rerr := os.ReadFile(file)
		require.NoError(t, rerr)

		sawRestored = string(data)

		return nil
	}

	require.NoError(t, s.Restore())
	assert.Equal(t, "old", sawRestored, "the reload runs once the files are back")
	assert.True(t, s.Restored())
}

func TestSnapshotRestoreKeepsTheCopyWhenTheReloadFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/containerd"), 0o755))

	s, err := TakeSnapshot(quiet(), filepath.Join(root, "snapshot"), root)
	require.NoError(t, err)

	s.reload = func() error { return errors.New("systemd is busy") }

	require.ErrorContains(t, s.Restore(), "systemd is busy")
	assert.False(t, s.Restored(), "a restore that could not be applied is tried again")
	assert.DirExists(t, filepath.Join(root, "snapshot"))
}

func TestSnapshotUnderATestRootDoesNotReloadTheRealSystemd(t *testing.T) {
	t.Parallel()

	s, err := TakeSnapshot(quiet(), filepath.Join(t.TempDir(), "snapshot"), t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, s.reload, "only a snapshot of the real root reloads systemd")
}

func TestTakeSnapshotReportsAFileItCannotSave(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, KubeletClientPKIDir), 0o755))

	// A socket cannot be saved byte for byte.
	l, err := net.Listen("unix", filepath.Join(root, KubeletClientPKIDir, "s"))
	require.NoError(t, err)

	defer l.Close() //nolint:errcheck // test cleanup

	_, err = TakeSnapshot(quiet(), filepath.Join(t.TempDir(), "snapshot"), root)
	require.ErrorContains(t, err, "save shared configuration")
}

func nodeStart(t *testing.T, mutate func(*goalstates.Kubelet)) (*goalstates.NodeStart, string) {
	t.Helper()

	root := t.TempDir()
	ns := &goalstates.NodeStart{RootOverride: isolation.DirRoot{SlotName: "kube1", Dir: root}}
	mutate(&ns.Kubelet)

	for _, p := range []string{goalstates.KubeletKubeconfigPath, KubeletClientPKIDir + "/kubelet-client-current.pem"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte("identity"), 0o600))
	}

	return ns, root
}

func TestResetKubeletIdentityRemovesCredentialsForTokenBootstrap(t *testing.T) {
	t.Parallel()

	ns, root := nodeStart(t, func(k *goalstates.Kubelet) { k.BootstrapToken = "abc.def" })

	require.NoError(t, ResetKubeletIdentity(ns))
	assert.NoFileExists(t, filepath.Join(root, goalstates.KubeletKubeconfigPath))
	assert.NoDirExists(t, filepath.Join(root, KubeletClientPKIDir))
}

func TestResetKubeletIdentityKeepsCredentialsConfigureRewrites(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*goalstates.Kubelet){
		"no bootstrap token": func(*goalstates.Kubelet) {},
		"direct kubeconfig":  func(k *goalstates.Kubelet) { k.BootstrapToken = "t"; k.KubeconfigData = []byte("kc") },
		"exec credential": func(k *goalstates.Kubelet) {
			k.BootstrapToken = "t"
			k.ExecCredential = &clientcmdapi.ExecConfig{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ns, root := nodeStart(t, mutate)

			require.NoError(t, ResetKubeletIdentity(ns))
			assert.FileExists(t, filepath.Join(root, goalstates.KubeletKubeconfigPath))
			assert.DirExists(t, filepath.Join(root, KubeletClientPKIDir))
		})
	}
}

func TestOrDefaultAndLogger(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/a", OrDefault("/a", "/b"))
	assert.Equal(t, "/b", OrDefault("", "/b"))

	l := quiet()
	assert.Same(t, l, Logger(l))
	assert.Same(t, slog.Default(), Logger(nil))
}
