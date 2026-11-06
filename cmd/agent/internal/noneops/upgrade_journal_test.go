// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// A failed upgrade leaves neither a journal nor a saved configuration.
func TestUpgradeFailureRemovesTheJournal(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	f.hook.kubeletErr = errors.New("kubelet crashlooping")

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "rolled back")
	assert.NoFileExists(t, f.node.journalPath())
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

// An upgrade does not start over the journal of one that was interrupted: that
// one has to be recovered first, or its saved configuration would be lost.
func TestUpgradeRefusesWhileAnInterruptedUpgradeIsUnrecovered(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: "1.32.0-bbbbbbbb", Target: "x"}))

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "interrupted")

	j, err := f.node.readJournal()
	require.NoError(t, err)
	assert.Equal(t, "1.32.0-bbbbbbbb", j.Previous, "the earlier journal is untouched")
}

func TestRecoverUpgradeWithNoJournalDoesNothing(t *testing.T) {
	f := newFixture(t, false, nil)
	require.NoError(t, f.node.RecoverUpgrade(t.Context()))
	assert.Empty(t, f.h.calls)
}

// The agent stopped after the new version was flipped to but before the
// upgrade finished: the next start puts the previous version and its
// configuration back.
func TestRecoverUpgradeRollsBackAnInterruptedUpgrade(t *testing.T) {
	// The node that recovers is built from the applied config, which is still
	// the previous configuration; its version is installed and journaled.
	f := newFixture(t, false, nil)
	prevVersion, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, prevVersion)
	seedSharedConfig(t, f)

	_, err = f.node.p.Layout.Ensure("1.35.0-cccccccc")
	require.NoError(t, err)

	snap, err := hostnode.TakeSnapshot(quiet(), f.node.configSnapshotDir(), f.root)
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: prevVersion, Target: "1.35.0-cccccccc"}))

	// What the crashed upgrade left: the new link and new configuration.
	_, err = f.node.p.Layout.Flip("1.35.0-cccccccc")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.hostPath("/etc/unbounded/containerd/config.toml"), []byte("new"), 0o600))

	require.NoError(t, f.node.RecoverUpgrade(t.Context()))

	cur, err := f.node.p.Layout.Current()
	require.NoError(t, err)
	assert.Equal(t, prevVersion, cur)
	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/unbounded/containerd/config.toml"))
	assert.FileExists(t, filepath.Join(f.sys, none.UnitKubelet), "the units are written for the configuration the node runs")
	assert.True(t, f.h.active[none.UnitKubelet], "the previous version runs again")
	assert.Equal(t, 1, f.hook.cleanup)
	assert.NoFileExists(t, f.node.journalPath())
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

// The upgrade succeeded and the applied config was persisted, but the agent
// stopped before Commit: nothing is rolled back.
func TestRecoverUpgradeCommitsAnUpgradeThatHadSucceeded(t *testing.T) {
	f := newFixture(t, false, nil)
	v, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, v)

	require.NoError(t, os.MkdirAll(f.node.configSnapshotDir(), 0o700))
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: "1.33.0-aaaaaaaa", Target: v}))

	require.NoError(t, f.node.RecoverUpgrade(t.Context()))
	assert.Empty(t, f.h.calls, "the node is not touched")
	assert.NoFileExists(t, f.node.journalPath())
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

// A rollback that fails keeps the journal so the next start tries again.
func TestRecoverUpgradeKeepsTheJournalWhenTheRollbackFails(t *testing.T) {
	f := newFixture(t, false, nil)
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: "1.33.0-missing", Target: "x"}))

	require.ErrorContains(t, f.node.RecoverUpgrade(t.Context()), "roll back the interrupted upgrade")
	assert.FileExists(t, f.node.journalPath())
}

func TestRecoverUpgradeRefusesAnUnreadableJournal(t *testing.T) {
	f := newFixture(t, false, nil)
	require.NoError(t, os.MkdirAll(filepath.Dir(f.node.journalPath()), 0o700))
	require.NoError(t, os.WriteFile(f.node.journalPath(), []byte("{"), 0o600))

	require.ErrorContains(t, f.node.RecoverUpgrade(t.Context()), "decode upgrade journal")
}

// A rollback starts the previous version with the units and host networking of
// the configuration it ran: here LocalDNS was on, and the failed upgrade turned
// it off.
func TestUpgradeRollbackUsesThePreviousConfiguration(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	prevGS := hostnodetest.GoalState("kube1", true)
	prevGS.RootFS.KubernetesVersion = "v1.33.0"
	f.node.p.PreviousGoalState = prevGS

	f.hook.kubeletErr = errors.New("kubelet crashlooping")

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "rolled back to 1.33.0-aaaaaaaa")
	assert.FileExists(t, filepath.Join(f.sys, none.UnitLocalDNS), "the rollback wrote the previous LocalDNS unit back")
	assert.True(t, f.h.active[none.UnitLocalDNS], "and started it")
	assert.Equal(t, 2, f.hook.cleanup, "the previous version also starts on a cleaned host")
}

// A rollback waits on the LocalDNS listeners of the configuration it returns
// to. Waiting on the failed upgrade's would time out, and the previous version
// would never start containerd or kubelet.
func TestUpgradeRollbackWaitsOnThePreviousLocalDNSListeners(t *testing.T) {
	f := newFixture(t, true, func(gs *goalstates.MachineGoalState) {
		gs.NodeStart.LocalDNS.NodeListenerIP = netip.MustParseAddr("169.254.30.10")
	})
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	prevGS := hostnodetest.GoalState("kube1", true)
	prevGS.RootFS.KubernetesVersion = "v1.33.0"
	f.node.p.PreviousGoalState = prevGS

	f.hook.kubeletErr = errors.New("kubelet crashlooping")

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "rolled back to 1.33.0-aaaaaaaa")
	assert.Equal(t, []string{"169.254.30.10", "169.254.20.10"}, f.hook.dnsWaited,
		"the upgrade waits on its own listeners, the rollback on the previous ones")
	assert.True(t, f.h.active[none.UnitContainerd], "the previous version's containerd started")
	assert.True(t, f.h.active[none.UnitKubelet], "and its kubelet")
}
