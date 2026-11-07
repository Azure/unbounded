// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stubTool puts an executable shell script named name on PATH.
func stubTool(t *testing.T, name, script string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRootPaths(t *testing.T) {
	r := Root("kube1")
	require.Equal(t, "kube1", r.Slot())
	require.Equal(t, "/var/lib/machines/kube1/etc/foo", r.Path("/etc/foo"))
	require.Equal(t, "/var/lib/machines/kube1/etc/foo", r.Path("etc/foo"))
	require.Equal(t, "/x/y/z", RootAt("kube2", "/x/y").Path("/z"))
}

func TestModeIsNSpawn(t *testing.T) {
	require.Equal(t, isolation.ModeNSpawn, (&Backend{}).Mode())
}

func TestLifecycleTakesASlotName(t *testing.T) {
	var got []string

	b := &Backend{Ops: Ops{Stop: func(_ context.Context, r isolation.SlotRef) error {
		got = append(got, r.Slot())

		return nil
	}}}

	require.NoError(t, b.Stop(context.Background(), isolation.SlotName("kube2")))
	require.Equal(t, []string{"kube2"}, got)
}

func TestRequirements(t *testing.T) {
	req := (&Backend{}).Requirements()
	require.Contains(t, req.HostPackages, "systemd-container")
	require.Contains(t, req.HostCommands, "machinectl")
}

func TestExecUsesSystemdRunMachine(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "args")
	stubTool(t, "systemd-run", `echo "$@" > `+rec+"\necho hello\n")

	b := &Backend{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	res, err := b.Exec(context.Background(), Root("kube2"), isolation.ExecSpec{Args: []string{"systemctl", "stop", "kubelet"}})
	require.NoError(t, err)
	require.Equal(t, "hello", res.Stdout)

	got, err := os.ReadFile(rec)
	require.NoError(t, err)
	require.Equal(t, "--machine=kube2 --pipe --wait systemctl stop kubelet", strings.TrimSpace(string(got)))
}

func TestExecFailure(t *testing.T) {
	stubTool(t, "systemd-run", "exit 3\n")

	b := &Backend{}
	_, err := b.Exec(context.Background(), Root("kube1"), isolation.ExecSpec{Args: []string{"false"}})
	require.Error(t, err)
}

func TestStatus(t *testing.T) {
	b := &Backend{}

	stubTool(t, "systemd-run", "exit 0\n")

	st, err := b.Status(context.Background(), Root("kube1"))
	require.NoError(t, err)
	require.True(t, st.Running)

	stubTool(t, "systemd-run", "exit 1\n")

	st, err = b.Status(context.Background(), Root("kube1"))
	require.NoError(t, err)
	require.False(t, st.Running)
	require.NotEmpty(t, st.Detail)
}

func TestLifecycleOpsNotWired(t *testing.T) {
	b := &Backend{}
	ctx := context.Background()
	r := Root("kube1")

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.ErrorIs(t, err, ErrNotWired)
	require.ErrorIs(t, b.Start(ctx, r), ErrNotWired)
	require.ErrorIs(t, b.Stop(ctx, r), ErrNotWired)
	require.ErrorIs(t, b.Restart(ctx, r), ErrNotWired)
	require.ErrorIs(t, b.Destroy(ctx, r), ErrNotWired)
}

func TestLifecycleOpsDelegate(t *testing.T) {
	var calls []string

	rec := func(name string) func(context.Context, isolation.SlotRef) error {
		return func(_ context.Context, r isolation.SlotRef) error {
			calls = append(calls, name+" "+r.Slot())
			return nil
		}
	}

	b := &Backend{Ops: Ops{
		Prepare: func(_ context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
			calls = append(calls, "prepare "+n.Slot)
			return Root(n.Slot), nil
		},
		Start: rec("start"), Stop: rec("stop"), Restart: rec("restart"), Destroy: rec("destroy"),
	}}
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube2"})
	require.NoError(t, err)
	require.NoError(t, b.Start(ctx, r))
	require.NoError(t, b.Restart(ctx, r))
	require.NoError(t, b.Stop(ctx, r))
	require.NoError(t, b.Destroy(ctx, r))
	require.Equal(t, []string{"prepare kube2", "start kube2", "restart kube2", "stop kube2", "destroy kube2"}, calls)
}

func TestLogs(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "args")
	stubTool(t, "journalctl", `echo "$@" > `+rec+"\necho line1\n")

	rc, err := (&Backend{}).Logs(context.Background(), Root("kube1"), isolation.LogOptions{Unit: "kubelet", Follow: true, Lines: 5})
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "line1\n", string(data))

	got, err := os.ReadFile(rec)
	require.NoError(t, err)
	require.Equal(t, "--machine=kube1 --no-pager --unit=kubelet --follow --lines=5", strings.TrimSpace(string(got)))
}
