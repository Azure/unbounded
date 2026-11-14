// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestStateDirIsRequired(t *testing.T) {
	t.Parallel()

	for _, stateDir := range []string{"", "relative"} {
		t.Run(stateDir, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			f.node.p.StateDir = stateDir
			require.ErrorContains(t, f.node.EnsureBootGuard(t.Context()), "StateDir")
			_, err := f.node.PendingStartup()
			require.ErrorContains(t, err, "StateDir")
			_, err = f.node.PendingUpgrade()
			require.ErrorContains(t, err, "StateDir")
			require.ErrorContains(t, f.node.Provision(t.Context()), "StateDir")
			require.Empty(t, f.host.calls)
		})
	}
}

func TestLoggerIsRequired(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	f.node.p.Log = nil
	require.ErrorContains(t, f.node.EnsureBootGuard(t.Context()), "Log is required")
	_, err := f.node.PendingStartup()
	require.ErrorContains(t, err, "Log is required")
	require.ErrorContains(t, f.node.Provision(t.Context()), "Log is required")
	require.Empty(t, f.host.calls)
}

func TestUpgradeRequiresReplayableConfiguration(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	installOld(t, f)
	f.node.p.Config = nil
	require.ErrorContains(t, f.node.Upgrade(t.Context()), "requires Config")
	require.Empty(t, f.host.calls)
	pending, err := f.node.PendingUpgrade()
	require.NoError(t, err)
	require.False(t, pending)
}

func TestStartupRetainsCallerStateAcrossCompletion(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	installOld(t, f)
	f.node.p.Completion = json.RawMessage(`{"applicationState":{"generation":43},"unknown":[1,2,3]}`)
	f.node.p.ArtifactVersion = "application-v2"
	require.NoError(t, f.node.Upgrade(t.Context()))
	s, err := f.node.PendingStartup()
	require.NoError(t, err)
	require.NotNil(t, s)
	require.JSONEq(t, string(f.node.p.Completion), string(s.Completion))
	require.Equal(t, f.node.p.Generation, s.Generation)
	require.Equal(t, "application-v2", s.ArtifactVersion)
	require.False(t, s.Finished)

	// A different caller goal must still finish the frozen startup record.
	p := f.node.p
	p.Generation, p.ArtifactVersion, p.Completion = "new-goal", "application-v3", nil
	recovered := New(p)
	require.NoError(t, recovered.ResumeStartup(t.Context()))
	require.NoError(t, recovered.Commit())
	require.NoError(t, recovered.FinishStartup())
	s, err = recovered.PendingStartup()
	require.NoError(t, err)
	require.True(t, s.Finished)
	require.Equal(t, f.node.p.Generation, s.Generation)
	require.JSONEq(t, string(f.node.p.Completion), string(s.Completion))
	require.NoError(t, recovered.ClearStartup())
	s, err = recovered.PendingStartup()
	require.NoError(t, err)
	require.Nil(t, s)
	require.Error(t, recovered.FinishStartup())
}

func TestSourceStartupUsesPreviousCompletion(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	old := installOld(t, f)
	f.node.p.Previous = f.node.p.Config.DeepCopy()
	f.node.p.PreviousGeneration = "previous-generation"
	f.node.p.PreviousCompletion = json.RawMessage(`{"source":"preserve this state"}`)
	f.node.p.Completion = json.RawMessage(`{"target":"not applied"}`)
	f.node.p.Stage = func(context.Context) error { return errors.New("staging failed") }
	require.Error(t, f.node.Upgrade(t.Context()))
	s, err := f.node.PendingStartup()
	require.NoError(t, err)
	require.NotNil(t, s)
	require.True(t, s.Restored)
	require.Equal(t, old, s.Version)
	require.Equal(t, f.node.p.PreviousGeneration, s.Generation)
	require.JSONEq(t, string(f.node.p.PreviousCompletion), string(s.Completion))
}

func TestStartupRejectsChangedActiveVersionWithoutMutation(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	old := installOld(t, f)
	require.NoError(t, f.node.Upgrade(t.Context()))
	_, err := f.node.p.Layout.Flip(old)
	require.NoError(t, err)

	before := len(f.host.calls)
	require.ErrorContains(t, New(f.node.p).ResumeStartup(t.Context()), "refusing to switch live binaries")
	require.Len(t, f.host.calls, before)
	current, err := f.node.p.Layout.Current()
	require.NoError(t, err)
	require.Equal(t, old, current)
}

func TestCorruptStartupBlocksDestructiveRecovery(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	installOld(t, f)
	mustWrite(t, f.node.startupPath(), "{")
	require.ErrorContains(t, f.node.RecoverUpgrade(t.Context()), "decode none startup")
	require.Empty(t, f.host.calls)
}

func TestOrdinaryStopPreservesBootPermission(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	require.NoError(t, f.node.Provision(t.Context()))
	b, err := f.node.Backend()
	require.NoError(t, err)
	require.NoError(t, b.Stop(t.Context(), nil))
	require.NoError(t, runBootGuard(f))

	for _, call := range f.host.calls {
		require.NotContains(t, call, "systemctl disable")
		require.NotContains(t, call, "systemctl mask")
	}
}

func TestSnapshotOverrideDoesNotRelocateTransactionRecords(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	f.node.p.ConfigSnapshotDir = t.TempDir()
	require.NoError(t, f.node.EnsureBootGuard(t.Context()))
	require.Equal(t, filepath.Join(f.root, f.node.p.StateDir, "none-startup.json"), f.node.startupPath())
	data, err := os.ReadFile(filepath.Join(f.units, none.UnitKubelet+".d", "01-startup-guard.conf"))
	require.NoError(t, err)
	require.Contains(t, string(data), systemdArgument(f.node.journalPath()))
	require.Contains(t, string(data), systemdArgument(f.node.startupPath()))
}

func TestBootGuardEscapesControlCharacters(t *testing.T) {
	t.Parallel()

	got := systemdArgument("space dir/\"quote\"/\\path/$HOME/%i/\n\r\t")
	for _, escaped := range []string{`\"quote\"`, `\\path`, `$$HOME`, `%%i`, `\n\r\t`} {
		require.Contains(t, got, escaped)
	}

	require.False(t, strings.ContainsAny(got, "\n\r\t"))
}
