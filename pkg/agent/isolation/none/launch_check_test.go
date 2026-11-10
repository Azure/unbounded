// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

const testLaunchCheckCommand = "/opt/unbounded/bin/unbounded-agent allocation-launch-check"

func TestKubeletLaunchCheckGolden(t *testing.T) {
	isolationtest.Golden(t, UnitKubeletLaunchCheck, RenderKubeletLaunchCheck(testLaunchCheckCommand))
	isolationtest.Golden(t, "launch-check.conf", RenderKubeletLaunchCheckDropIn())
}

// The check is a oneshot whose final refusal is not restarted and is never a
// success: a SuccessExitStatus would let a refused kubelet start through.
func TestKubeletLaunchCheckUnit(t *testing.T) {
	t.Parallel()

	unit := string(RenderKubeletLaunchCheck(testLaunchCheckCommand))
	for _, want := range []string{
		"\nType=oneshot\n",
		"\nExecStart=" + testLaunchCheckCommand + "\n",
		"\nRestart=on-failure\n",
		"\nRestartMode=direct\n",
		"\nRestartSec=10s\n",
		"\nRestartPreventExitStatus=" + strconv.Itoa(goalstates.AllocationLaunchRefusedExitCode) + "\n",
	} {
		require.Contains(t, unit, want)
	}

	for _, unwanted := range []string{"SuccessExitStatus", "RemainAfterExit", "TimeoutStartSec", "[Install]"} {
		require.NotContains(t, unit, unwanted)
	}

	dropIn := string(RenderKubeletLaunchCheckDropIn())
	require.Contains(t, dropIn, "\nRequires="+UnitKubeletLaunchCheck+"\n")
	require.Contains(t, dropIn, "\nAfter="+UnitKubeletLaunchCheck+"\n")
	require.NotContains(t, dropIn, "ExecStartPre")
}

func TestKubeletLaunchCheckWriteDropIns(t *testing.T) {
	t.Parallel()

	launchCheck := func(dir string) string { return filepath.Join(dir, UnitKubeletLaunchCheck) }
	dropIn := func(dir string) string { return filepath.Join(DropInDir(dir, UnitKubelet), "10-launch-check.conf") }

	for _, localDNS := range []bool{true, false} {
		t.Run("localdns "+strconv.FormatBool(localDNS), func(t *testing.T) {
			t.Parallel()

			plainDir := t.TempDir()
			plain, err := WriteDropIns(plainDir, UnitSpec{BinDir: "/b/bin", LocalDNS: localDNS})
			require.NoError(t, err)

			dir := t.TempDir()
			paths, err := WriteDropIns(dir, UnitSpec{BinDir: "/b/bin", LocalDNS: localDNS, LaunchCheckCommand: testLaunchCheckCommand})
			require.NoError(t, err)
			require.Len(t, paths, len(plain)+2)
			require.Contains(t, paths, launchCheck(dir))
			require.Contains(t, paths, dropIn(dir))

			unit, err := os.ReadFile(launchCheck(dir))
			require.NoError(t, err)
			require.Equal(t, string(RenderKubeletLaunchCheck(testLaunchCheckCommand)), string(unit))

			conf, err := os.ReadFile(dropIn(dir))
			require.NoError(t, err)
			require.Equal(t, string(RenderKubeletLaunchCheckDropIn()), string(conf))
		})
	}

	t.Run("no command into an empty directory writes neither", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		paths, err := WriteDropIns(dir, UnitSpec{BinDir: "/b/bin", LocalDNS: true})
		require.NoError(t, err)
		require.Len(t, paths, 6, "the same paths as without the launch check")
		require.NoFileExists(t, launchCheck(dir))
		require.NoFileExists(t, dropIn(dir))
	})

	t.Run("no command never takes the check away", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		_, err := WriteDropIns(dir, UnitSpec{BinDir: "/b/bin", LaunchCheckCommand: testLaunchCheckCommand})
		require.NoError(t, err)

		_, err = WriteDropIns(dir, UnitSpec{BinDir: "/b/bin"})
		require.NoError(t, err)
		require.FileExists(t, launchCheck(dir))
		require.FileExists(t, dropIn(dir))
	})
}

func newLaunchCheckBackend(t *testing.T) (*Backend, *recorder) {
	t.Helper()

	b, rec := newBackend(t)
	b.Units.LaunchCheckCommand = testLaunchCheckCommand

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubeletLaunchCheck))
	require.FileExists(t, filepath.Join(DropInDir(b.SystemdDir, UnitKubelet), "10-launch-check.conf"))

	return b, rec
}

func TestDestroyRemovesTheKubeletLaunchCheck(t *testing.T) {
	b, rec := newLaunchCheckBackend(t)
	rec.calls = nil

	// The check is removed only once the kubelet unit and its drop-ins are gone.
	stopCheck := "systemctl stop " + UnitKubeletLaunchCheck
	b.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if call := name + " " + strings.Join(args, " "); call == stopCheck {
			require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
			require.NoDirExists(t, DropInDir(b.SystemdDir, UnitKubelet))
			require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubeletLaunchCheck))
		}

		return rec.run(ctx, log, name, args...)
	}

	require.NoError(t, b.Destroy(context.Background(), root("v1")))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubeletLaunchCheck))
	require.NoDirExists(t, DropInDir(b.SystemdDir, UnitKubelet))
	require.Contains(t, rec.calls, stopCheck)
	require.NotContains(t, rec.calls, "systemctl disable "+UnitKubeletLaunchCheck, "the check has no [Install] section")
	require.Equal(t, "systemctl daemon-reload", rec.calls[len(rec.calls)-1])
}

// A kubelet that keeps running is still checked when it restarts.
func TestDestroyKeepsTheLaunchCheckOfAKubeletThatDidNotStop(t *testing.T) {
	b, rec := newLaunchCheckBackend(t)
	rec.fail["systemctl stop "+UnitKubelet] = errors.New("stuck")

	require.ErrorContains(t, b.Destroy(context.Background(), root("v1")), "stuck")
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubeletLaunchCheck))
	require.FileExists(t, filepath.Join(DropInDir(b.SystemdDir, UnitKubelet), "10-launch-check.conf"))
	require.NotContains(t, rec.calls, "systemctl stop "+UnitKubeletLaunchCheck)
}

// A check that did not stop keeps its file, as the node units do.
func TestDestroyKeepsALaunchCheckThatDidNotStop(t *testing.T) {
	b, rec := newLaunchCheckBackend(t)
	rec.fail["systemctl stop "+UnitKubeletLaunchCheck] = errors.New("stuck")

	require.ErrorContains(t, b.Destroy(context.Background(), root("v1")), "stuck")
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubeletLaunchCheck))
}
