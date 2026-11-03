// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeStartRoot(t *testing.T) {
	n := &NodeStart{MachineName: "kube1", MachineDir: "/var/lib/machines/kube1"}
	require.Equal(t, "kube1", n.Root().Slot())
	require.Equal(t, filepath.Join(n.MachineDir, "etc/foo"), n.Root().Path("/etc/foo"))
}

func TestRootFSRoot(t *testing.T) {
	r := &RootFS{MachineDir: "/var/lib/machines/kube2"}
	require.Equal(t, "kube2", r.Root().Slot())
	require.Equal(t, "/var/lib/machines/kube2/usr/local/bin", r.Root().Path("usr/local/bin"))
}

func TestNodeStartExecTargetsMachine(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "args")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemd-run"), []byte("#!/bin/sh\necho \"$@\" > "+rec+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	n := &NodeStart{MachineName: "kube2"}
	_, err := n.Exec(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "ldconfig")
	require.NoError(t, err)

	got, err := os.ReadFile(rec)
	require.NoError(t, err)
	require.Equal(t, "--machine=kube2 --pipe --wait ldconfig\n", string(got))
}
