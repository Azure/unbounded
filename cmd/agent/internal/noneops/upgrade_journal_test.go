// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/config"
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

// withAppliedConfig gives the node the agent configuration it was built from,
// which the journal identifies the upgrade's configuration by.
func withAppliedConfig(f *fixture, version string) *config.AgentConfig {
	cfg := &config.AgentConfig{MachineName: "worker-1"}
	cfg.Cluster.Version = version
	f.node.p.Config = cfg

	return cfg
}

// stackCalls are the host calls that start or stop a unit.
func stackCalls(h *host) []string {
	var out []string

	for _, c := range h.calls {
		if strings.HasPrefix(c, "systemctl enable") || strings.HasPrefix(c, "systemctl stop") || strings.HasPrefix(c, "systemctl restart") {
			out = append(out, c)
		}
	}

	return out
}

// The upgrade succeeded and the applied config was persisted, but the agent
// stopped before Commit: nothing is rolled back, and a running stack is left
// alone.
func TestRecoverUpgradeCommitsAnUpgradeThatHadSucceeded(t *testing.T) {
	f := newFixture(t, false, nil)
	cfg := withAppliedConfig(f, "1.34.1")
	v, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, v)

	f.h.active[none.UnitContainerd] = true
	f.h.active[none.UnitKubelet] = true

	require.NoError(t, os.MkdirAll(f.node.configSnapshotDir(), 0o700))
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: "1.33.0-aaaaaaaa", Target: v, TargetConfig: configDigest(cfg)}))

	require.NoError(t, f.node.RecoverUpgrade(t.Context()))
	assert.Empty(t, stackCalls(f.h), "the node is not touched")
	assert.NoFileExists(t, f.node.journalPath())
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

// A repave that changes only configuration keeps the version name, so the
// journal's versions are equal. The agent was killed after the upgrade had
// stopped the stack and before the new configuration was persisted: the
// applied config is still the previous one, so the node is rolled back to it
// and started, not committed with its stack down.
func TestRecoverUpgradeOfAConfigOnlyRepaveRollsBackAndStartsTheStack(t *testing.T) {
	f := newFixture(t, false, nil)
	withAppliedConfig(f, "1.34.1")
	v, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, v)
	seedSharedConfig(t, f)

	snap, err := hostnode.TakeSnapshot(quiet(), f.node.configSnapshotDir(), f.root)
	require.NoError(t, err)
	require.NotNil(t, snap)

	target := &config.AgentConfig{MachineName: "worker-1"}
	target.Cluster.Version = "1.34.1"
	target.Kubelet.Labels = map[string]string{"new": "label"}
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: v, Target: v, TargetConfig: configDigest(target)}))

	// What the killed upgrade left: the new configuration and a stopped stack.
	require.NoError(t, os.WriteFile(f.hostPath("/etc/unbounded/containerd/config.toml"), []byte("new"), 0o600))
	require.Empty(t, f.h.active)

	require.NoError(t, f.node.RecoverUpgrade(t.Context()))

	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/unbounded/containerd/config.toml"), "the configuration the applied config describes is back")
	assert.True(t, f.h.active[none.UnitContainerd], "the stack runs again")
	assert.True(t, f.h.active[none.UnitKubelet])
	assert.NoFileExists(t, f.node.journalPath())
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

// The same repave, killed after the new configuration was persisted: the
// upgrade had succeeded, so it is committed, and the stack it found stopped is
// started.
func TestRecoverUpgradeOfAPersistedConfigOnlyRepaveStartsTheStack(t *testing.T) {
	f := newFixture(t, false, nil)
	cfg := withAppliedConfig(f, "1.34.1")
	v, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, v)

	require.NoError(t, os.MkdirAll(f.node.configSnapshotDir(), 0o700))
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: v, Target: v, TargetConfig: configDigest(cfg)}))

	require.NoError(t, f.node.RecoverUpgrade(t.Context()))
	assert.Equal(t, []string{"systemctl enable --now " + none.UnitContainerd, "systemctl enable --now " + none.UnitKubelet}, stackCalls(f.h))
	assert.Equal(t, 0, f.hook.cleanup, "a finished upgrade is not rolled back")
	assert.NoFileExists(t, f.node.journalPath())
}

// A finished upgrade whose stack does not start keeps the journal, so the next
// start or repave tries again.
func TestRecoverUpgradeKeepsTheJournalWhenTheFinishedStackDoesNotStart(t *testing.T) {
	f := newFixture(t, false, nil)
	cfg := withAppliedConfig(f, "1.34.1")
	v, err := f.node.Version()
	require.NoError(t, err)
	installPrevious(t, f, v)
	require.NoError(t, f.node.writeJournal(upgradeJournal{Previous: v, Target: v, TargetConfig: configDigest(cfg)}))

	f.h.fail["systemctl enable --now "+none.UnitContainerd] = errors.New("no")

	require.ErrorContains(t, f.node.RecoverUpgrade(t.Context()), "start the upgraded node")
	assert.FileExists(t, f.node.journalPath())
}

// Upgrade records the configuration it applies, the one its caller persists.
func TestUpgradeJournalsTheDigestOfTheConfigurationItApplies(t *testing.T) {
	f := newFixture(t, false, nil)
	cfg := withAppliedConfig(f, "1.34.1")
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	require.NoError(t, f.node.Upgrade(t.Context()))

	j, err := f.node.readJournal()
	require.NoError(t, err)
	assert.Equal(t, configDigest(cfg), j.TargetConfig)
	assert.NotEmpty(t, j.TargetConfig)

	// The applied config is read back from its JSON; the digest survives that.
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var read config.AgentConfig
	require.NoError(t, json.Unmarshal(data, &read))
	assert.Equal(t, j.TargetConfig, configDigest(&read))
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
