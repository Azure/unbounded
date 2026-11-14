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

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type fakeContainerNode struct {
	params     containerops.Params
	events     *[]string
	restart    error
	rewrite    error
	upgrade    error
	destroy    error
	upgradeOld ContainerNode
}

func (f *fakeContainerNode) rec(s string) { *f.events = append(*f.events, s+":"+f.params.Slot) }

func (f *fakeContainerNode) Restart(context.Context) error { f.rec("restart"); return f.restart }

func (f *fakeContainerNode) RewriteUnit(context.Context) error {
	f.rec("rewrite")
	return f.rewrite
}

func (f *fakeContainerNode) Upgrade(_ context.Context, old ContainerNode) error {
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

// newFakeContainerOperator returns an operator whose host-touching steps are
// recorded rather than run.
func newFakeContainerOperator() (*ContainerOperator, *containerStub) {
	s := &containerStub{events: &[]string{}, nodes: map[string]*fakeContainerNode{}}

	op := &ContainerOperator{
		NewNode: func(p containerops.Params) ContainerNode {
			n := &fakeContainerNode{params: p, events: s.events}
			if s.configure != nil {
				s.configure(p.Slot, n)
			}

			s.nodes[p.Slot] = n

			return n
		},
		ResolveDownloads: func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
			*s.events = append(*s.events, "resolve")
			return &goalstates.DownloadOverrides{}, nil, nil
		},
		StageArchives: func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
			*s.events = append(*s.events, "stage")
			return nil
		},
		Persist: func(_ context.Context, _ *slog.Logger, slot string, cfg *provision.AgentConfig) error {
			*s.events = append(*s.events, "persist:"+slot+":"+cfg.Isolation+":"+cfg.Cluster.Version)
			return nil
		},
		Remove: func(_ context.Context, _ *slog.Logger, slot string) error {
			*s.events = append(*s.events, "remove-applied:"+slot)
			return nil
		},
	}

	return op, s
}

func containerActive(slot string) *ActiveMachine {
	cfg := baseConfig()
	cfg.Isolation = "container"
	cfg.Experimental = &provision.ExperimentalConfig{ContainerIsolation: true}

	return &ActiveMachine{Name: slot, Config: cfg, Mode: isolation.ModeContainer}
}

func TestContainerOperatorRestartNode(t *testing.T) {
	op, s := newFakeContainerOperator()

	active := containerActive("kube1")
	before := *active.Config

	require.NoError(t, op.RestartNode(t.Context(), discardLogger(), active))
	assert.Equal(t, []string{"restart:kube1"}, *s.events)
	assert.Equal(t, "container", s.nodes["kube1"].params.Config.Isolation)
	assert.Equal(t, before, *active.Config, "the applied config is not mutated")

	s.configure = func(_ string, n *fakeContainerNode) { n.restart = errors.New("kubelet not active") }
	err := op.RestartNode(t.Context(), discardLogger(), active)
	require.ErrorContains(t, err, "kubelet not active")
	require.ErrorContains(t, err, "restart node")
}

func TestContainerOperatorLifecycleMigrationRewritesTheUnit(t *testing.T) {
	op, s := newFakeContainerOperator()

	require.NoError(t, op.EnsureLifecycleMigration(t.Context(), discardLogger(), containerActive("kube2")))
	assert.Equal(t, []string{"rewrite:kube2"}, *s.events)

	s.configure = func(_ string, n *fakeContainerNode) { n.rewrite = errors.New("daemon-reload failed") }
	err := op.EnsureLifecycleMigration(t.Context(), discardLogger(), containerActive("kube1"))
	require.ErrorContains(t, err, "daemon-reload failed")
	require.ErrorContains(t, err, "reconcile container unit")
}

func TestContainerOperatorRepaveMovesToTheAlternateSlot(t *testing.T) {
	for _, tc := range []struct{ oldSlot, newSlot string }{{"kube1", "kube2"}, {"kube2", "kube1"}} {
		t.Run(tc.oldSlot, func(t *testing.T) {
			op, s := newFakeContainerOperator()

			newCfg := &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
			newCfg.Cluster.Version = "1.34.0"
			// The desired config asks for another mode; the repave reconciler
			// handles that before it gets here, so the record keeps the running mode.
			newCfg.Isolation = "none"

			require.NoError(t, op.RepaveNode(t.Context(), discardLogger(), containerActive(tc.oldSlot), newCfg))

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
	op, s := newFakeContainerOperator()
	s.configure = func(slot string, n *fakeContainerNode) {
		if slot == "kube2" {
			n.upgrade = errors.New("start slot kube2: unit failed (previous slot kube1 restored)")
		}
	}

	err := op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "previous slot kube1 restored")
	assert.Equal(t, []string{"resolve", "stage", "upgrade:kube2"}, *s.events, "the record still describes what is running")
}

func TestContainerOperatorRepaveRetireFailureIsNotARepaveFailure(t *testing.T) {
	op, s := newFakeContainerOperator()
	s.configure = func(slot string, n *fakeContainerNode) {
		if slot == "kube1" {
			n.destroy = errors.New("still mounted")
		}
	}

	require.NoError(t, op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}))
	assert.Contains(t, strings.Join(*s.events, ","), "remove-applied:kube1", "the record is already on the new slot")
}

func TestContainerOperatorRepaveSetupAndRecordFailures(t *testing.T) {
	newCfg := func() *provision.UnboundedAgentConfig {
		return &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
	}

	op, s := newFakeContainerOperator()
	op.ResolveDownloads = func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		return nil, nil, errors.New("bad offline bundle")
	}

	err := op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "resolve download overrides")
	assert.Empty(t, *s.events)

	op, s = newFakeContainerOperator()
	op.StageArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		return errors.New("staging failed")
	}

	err = op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "staging failed")
	assert.Empty(t, s.nodes, "nothing is built or stopped when staging fails")

	op, s = newFakeContainerOperator()
	op.Persist = func(context.Context, *slog.Logger, string, *provision.AgentConfig) error {
		return errors.New("read-only /etc")
	}

	err = op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "read-only /etc")
	assert.NotContains(t, strings.Join(*s.events, ","), "remove-applied", "the old record stays until the new one is written")

	op, s = newFakeContainerOperator()
	op.Remove = func(context.Context, *slog.Logger, string) error { return errors.New("cannot remove") }

	err = op.RepaveNode(t.Context(), discardLogger(), containerActive("kube1"), newCfg())
	require.ErrorContains(t, err, "cannot remove")
	assert.NotContains(t, strings.Join(*s.events, ","), "destroy", "the old slot's files stay while its record does")
}

func TestRealContainerNodeRejectsAForeignOldNode(t *testing.T) {
	n := realContainerNode{containerops.New(containerops.Params{Slot: "kube2"})}
	require.ErrorContains(t, n.Upgrade(t.Context(), &fakeContainerNode{}), "cannot upgrade from")
}

func TestContainerOperatorDefaultsAreTheRealNode(t *testing.T) {
	_, ok := ContainerOperator{}.node(containerops.Params{Log: discardLogger(), Slot: "kube1"}).(realContainerNode)
	assert.True(t, ok)
}
