// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

type fakeContainerNode struct {
	params     containerops.Params
	events     *[]string
	restart    error
	rewrite    error
	upgrade    error
	destroy    error
	upgradeOld containerNode
}

func (f *fakeContainerNode) rec(s string) { *f.events = append(*f.events, s+":"+f.params.Slot) }

func (f *fakeContainerNode) Restart(context.Context) error { f.rec("restart"); return f.restart }

func (f *fakeContainerNode) RewriteUnit(context.Context) error {
	f.rec("rewrite")
	return f.rewrite
}

func (f *fakeContainerNode) Upgrade(_ context.Context, old containerNode) error {
	f.rec("upgrade")

	f.upgradeOld = old

	return f.upgrade
}

func (f *fakeContainerNode) Destroy(context.Context) error { f.rec("destroy"); return f.destroy }

type containerStub struct {
	events *[]string
	nodes  map[string]*fakeContainerNode // by slot
	// configure is applied to every node the operator creates.
	configure func(slot string, n *fakeContainerNode)
}

// stubContainer replaces the host-touching seams and returns the recorder.
func stubContainer(t *testing.T) *containerStub {
	t.Helper()

	s := &containerStub{events: &[]string{}, nodes: map[string]*fakeContainerNode{}}

	oldNew := newContainerNode

	t.Cleanup(func() { newContainerNode = oldNew })

	stubNodeSeams(t, s.events)

	newContainerNode = func(p containerops.Params) containerNode {
		n := &fakeContainerNode{params: p, events: s.events}
		if s.configure != nil {
			s.configure(p.Slot, n)
		}

		s.nodes[p.Slot] = n

		return n
	}
	persistNodeConfig = func(_ context.Context, _ *slog.Logger, slot string, cfg *provision.AgentConfig) error {
		*s.events = append(*s.events, "persist:"+slot+":"+cfg.Isolation+":"+cfg.Cluster.Version)
		return nil
	}
	removeNodeConfig = func(_ context.Context, _ *slog.Logger, slot string) error {
		*s.events = append(*s.events, "remove-applied:"+slot)
		return nil
	}

	return s
}

func containerActive(slot string) *ActiveMachine {
	cfg := baseConfig()
	cfg.Isolation = "container"

	return &ActiveMachine{Name: slot, Config: cfg, Mode: isolation.ModeContainer}
}

func TestContainerOperatorIsRegisteredAndDispatched(t *testing.T) {
	op, ok := newNodeOperator().(*dispatchOperator)
	require.True(t, ok)

	got, err := op.forMode(isolation.ModeContainer)
	require.NoError(t, err)
	assert.IsType(t, containerNodeOperator{}, got)

	assert.Panics(t, func() {
		registerNodeOperator(isolation.ModeContainer, func() nodeOperator { return containerNodeOperator{} })
	})

	s := stubContainer(t)
	require.NoError(t, newNodeOperator().RestartNode(t.Context(), discardLogger(), containerActive("kube2")))
	assert.Equal(t, []string{"restart:kube2"}, *s.events)
}

func TestContainerOperatorRestartNode(t *testing.T) {
	s := stubContainer(t)

	active := containerActive("kube1")
	before := *active.Config

	require.NoError(t, containerNodeOperator{}.RestartNode(t.Context(), discardLogger(), active))
	assert.Equal(t, []string{"restart:kube1"}, *s.events)
	assert.Equal(t, "container", s.nodes["kube1"].params.Config.Isolation)
	assert.Equal(t, before, *active.Config, "the applied config is not mutated")

	s.configure = func(_ string, n *fakeContainerNode) { n.restart = errors.New("kubelet not active") }
	err := containerNodeOperator{}.RestartNode(t.Context(), discardLogger(), active)
	require.ErrorContains(t, err, "kubelet not active")
	require.ErrorContains(t, err, "restart node")
}

func TestContainerOperatorLifecycleMigrationRewritesTheUnit(t *testing.T) {
	s := stubContainer(t)

	require.NoError(t, containerNodeOperator{}.EnsureLifecycleMigration(t.Context(), discardLogger(), containerActive("kube2")))
	assert.Equal(t, []string{"rewrite:kube2"}, *s.events)

	s.configure = func(_ string, n *fakeContainerNode) { n.rewrite = errors.New("daemon-reload failed") }
	err := containerNodeOperator{}.EnsureLifecycleMigration(t.Context(), discardLogger(), containerActive("kube1"))
	require.ErrorContains(t, err, "daemon-reload failed")
	require.ErrorContains(t, err, "reconcile container unit")
}

func TestContainerOperatorRepaveMovesToTheAlternateSlot(t *testing.T) {
	for _, tc := range []struct{ oldSlot, newSlot string }{{"kube1", "kube2"}, {"kube2", "kube1"}} {
		t.Run(tc.oldSlot, func(t *testing.T) {
			s := stubContainer(t)

			newCfg := &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
			newCfg.Cluster.Version = "1.34.0"
			// The desired config asks for another mode; the repave reconciler
			// handles that before it gets here, so the record keeps the running mode.
			newCfg.Isolation = "none"

			require.NoError(t, containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive(tc.oldSlot), newCfg))

			// The record moves to the new slot before the old slot's files go.
			assert.Equal(t, []string{
				"resolve", "stage",
				"upgrade:" + tc.newSlot,
				"persist:" + tc.newSlot + ":container:1.34.0",
				"remove-applied:" + tc.oldSlot,
				"destroy:" + tc.oldSlot,
			}, *s.events)

			assert.Equal(t, "container", s.nodes[tc.newSlot].params.Config.Isolation)
			assert.Equal(t, "none", newCfg.Isolation, "the caller's desired config is not modified")
			require.NotNil(t, s.nodes[tc.newSlot].params.Downloads)
			assert.Same(t, s.nodes[tc.oldSlot], s.nodes[tc.newSlot].upgradeOld, "the new slot replaces the old node")
		})
	}
}

func TestContainerOperatorRepaveFailureRecordsNothing(t *testing.T) {
	s := stubContainer(t)
	s.configure = func(slot string, n *fakeContainerNode) {
		if slot == "kube2" {
			n.upgrade = errors.New("start slot kube2: unit failed (previous slot kube1 restored)")
		}
	}

	err := containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "previous slot kube1 restored")
	assert.Equal(t, []string{"resolve", "stage", "upgrade:kube2"}, *s.events, "the record still describes what is running")
}

func TestContainerOperatorRepaveRetireFailureIsNotARepaveFailure(t *testing.T) {
	s := stubContainer(t)
	s.configure = func(slot string, n *fakeContainerNode) {
		if slot == "kube1" {
			n.destroy = errors.New("still mounted")
		}
	}

	require.NoError(t, containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}))
	assert.Contains(t, strings.Join(*s.events, ","), "remove-applied:kube1", "the record is already on the new slot")
}

func TestContainerOperatorRepaveSetupAndRecordFailures(t *testing.T) {
	newCfg := func() *provision.UnboundedAgentConfig {
		return &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
	}

	s := stubContainer(t)
	resolveNodeDownloads = func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		return nil, nil, errors.New("bad offline bundle")
	}

	err := containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "resolve download overrides")
	assert.Empty(t, *s.events)

	s = stubContainer(t)
	stageNodeArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		return errors.New("staging failed")
	}

	err = containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "staging failed")
	assert.Empty(t, s.nodes, "nothing is built or stopped when staging fails")

	s = stubContainer(t)
	persistNodeConfig = func(context.Context, *slog.Logger, string, *provision.AgentConfig) error {
		return errors.New("read-only /etc")
	}

	err = containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "read-only /etc")
	assert.NotContains(t, strings.Join(*s.events, ","), "remove-applied", "the old record stays until the new one is written")

	s = stubContainer(t)
	removeNodeConfig = func(context.Context, *slog.Logger, string) error { return errors.New("cannot remove") }

	err = containerNodeOperator{}.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "cannot remove")
	assert.NotContains(t, strings.Join(*s.events, ","), "destroy", "the old slot's files stay while its record does")
}

func TestContainerOperatorMigrationToAnUnknownTargetNamesTheMode(t *testing.T) {
	desired := &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
	desired.Isolation = "bogus"

	err := containerNodeOperator{}.MigrateIsolation(t.Context(), discardLogger(), containerActive("kube1"), desired, migrate.Strategy("reprovision"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "container")
}

func TestRealContainerNodeRejectsAForeignOldNode(t *testing.T) {
	n := realContainerNode{containerops.New(containerops.Params{Slot: "kube2"})}
	require.ErrorContains(t, n.Upgrade(t.Context(), &fakeContainerNode{}), "cannot upgrade from")
}

func TestResetResourcesChoosesTheContainerTeardownByHost(t *testing.T) {
	oldNone, oldContainer := hostIsNone, hostIsContainer

	t.Cleanup(func() { hostIsNone, hostIsContainer = oldNone, oldContainer })

	hostIsNone = func() bool { return false }
	hostIsContainer = func() bool { return true }

	name := resetResources(discardLogger()).Name()
	assert.NotContains(t, name, "stop-machine")
	assert.NotContains(t, name, "remove-bpffs-mount", "the container uses the host's bpffs")
	assert.NotContains(t, name, "none-node")

	// The order of the nspawn reset is preserved: daemon unit, stop, WireGuard
	// keys, config, root, network and routes, bootstrap unit, artifacts, reload.
	order := []string{
		"remove-daemon-unit", "stop-container-node", "remove-wireguard-keys", "remove-container-config", "remove-container-node",
		"cleanup-routes", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	}

	last := -1

	for _, step := range order {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}

	// None wins when both look present, so an existing none host is never torn
	// down as a container host.
	hostIsNone = func() bool { return true }

	assert.Contains(t, resetResources(discardLogger()).Name(), "stop-none-node")

	hostIsNone = func() bool { return false }
	hostIsContainer = func() bool { return false }

	assert.Contains(t, resetResources(discardLogger()).Name(), "stop-machine")
}
