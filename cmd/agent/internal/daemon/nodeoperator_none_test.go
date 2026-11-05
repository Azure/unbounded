// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type fakeNoneNode struct {
	params  noneops.Params
	events  *[]string
	recover error
	restart error
	upgrade error
	commit  error
	prune   error
}

func (f *fakeNoneNode) record(e string) { *f.events = append(*f.events, e) }

func (f *fakeNoneNode) RecoverUpgrade(context.Context) error { f.record("recover"); return f.recover }

func (f *fakeNoneNode) Restart(context.Context) error { f.record("restart"); return f.restart }

func (f *fakeNoneNode) Upgrade(context.Context) error { f.record("upgrade"); return f.upgrade }
func (f *fakeNoneNode) Commit() error                 { f.record("commit"); return f.commit }
func (f *fakeNoneNode) PruneVersions() error          { f.record("prune"); return f.prune }

func newFakeNoneOperator() (NoneOperator, *fakeNoneNode, *[]string) {
	events := &[]string{}
	node := &fakeNoneNode{events: events}

	op := NoneOperator{
		NewNode: func(p noneops.Params) NoneNode { node.params = p; return node },
		ResolveDownloads: func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
			*events = append(*events, "resolve")
			return nil, nil, nil
		},
		StageArchives: func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
			*events = append(*events, "stage")
			return nil
		},
		Persist: func(_ context.Context, _ *slog.Logger, cfg *provision.AgentConfig) error {
			*events = append(*events, "persist:"+cfg.Isolation+":"+cfg.Cluster.Version)
			return nil
		},
	}

	return op, node, events
}

func noneActive() *ActiveMachine {
	cfg := baseConfig()
	cfg.Isolation = "none"

	return &ActiveMachine{Name: "kube1", Config: cfg, Mode: isolation.ModeNone}
}

func noneRepaveConfig(version string) *provision.UnboundedAgentConfig {
	cfg := baseConfig()
	cfg.Isolation = "none"
	cfg.Cluster.Version = version

	return &provision.UnboundedAgentConfig{AgentConfig: *cfg}
}

// The applied config is persisted only after the upgrade is healthy, the
// upgrade is committed after that, and versions are pruned last. The rollback
// gets the configuration the node ran.
func TestNoneOperatorRepaveOrder(t *testing.T) {
	op, node, events := newFakeNoneOperator()
	active := noneActive()

	require.NoError(t, op.RepaveNode(t.Context(), discardLogger(), active, noneRepaveConfig("1.35.0")))
	assert.Equal(t, []string{"resolve", "stage", "upgrade", "persist:none:1.35.0", "commit", "prune"}, *events)
	require.NotNil(t, node.params.Previous)
	assert.Equal(t, active.Config.Cluster.Version, node.params.Previous.Cluster.Version)
	assert.Equal(t, "1.35.0", node.params.Config.Cluster.Version)
}

// A failed upgrade persists nothing and commits nothing.
func TestNoneOperatorRepaveFailureLeavesTheAppliedConfig(t *testing.T) {
	op, node, events := newFakeNoneOperator()
	node.upgrade = errors.New("rolled back")

	require.ErrorContains(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), noneRepaveConfig("1.35.0")), "rolled back")
	assert.Equal(t, []string{"resolve", "stage", "upgrade"}, *events)
}

// A persist failure is reported and the upgrade is not committed, so the next
// start rolls back to the version the applied config describes.
func TestNoneOperatorRepavePersistFailureDoesNotCommit(t *testing.T) {
	op, _, events := newFakeNoneOperator()
	op.Persist = func(context.Context, *slog.Logger, *provision.AgentConfig) error { return errors.New("disk full") }

	require.ErrorContains(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), noneRepaveConfig("1.35.0")), "disk full")
	assert.NotContains(t, *events, "commit")
}

// Commit and prune failures happen after the node is healthy and persisted;
// they are logged, not returned.
func TestNoneOperatorRepaveCommitAndPruneFailuresAreNotFatal(t *testing.T) {
	op, node, _ := newFakeNoneOperator()
	node.commit = errors.New("busy")
	node.prune = errors.New("busy")

	require.NoError(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), noneRepaveConfig("1.35.0")))
}

// The repave keeps the node in none whatever the desired config says.
func TestNoneOperatorRepaveKeepsTheMode(t *testing.T) {
	op, node, _ := newFakeNoneOperator()

	cfg := noneRepaveConfig("1.35.0")
	cfg.Isolation = ""

	require.NoError(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), cfg))
	assert.Equal(t, "none", node.params.Config.Isolation)
}

func TestNoneOperatorRepaveResolveAndStageFailures(t *testing.T) {
	op, _, events := newFakeNoneOperator()
	op.ResolveDownloads = func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		return nil, nil, errors.New("bad override")
	}

	require.ErrorContains(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), noneRepaveConfig("1.35.0")), "resolve download overrides")
	assert.Empty(t, *events)

	op, _, events = newFakeNoneOperator()
	op.StageArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		return errors.New("offline")
	}

	require.ErrorContains(t, op.RepaveNode(t.Context(), discardLogger(), noneActive(), noneRepaveConfig("1.35.0")), "offline")
	assert.NotContains(t, *events, "upgrade")
}

// A soft reboot restarts the stack; the host is never rebooted.
func TestNoneOperatorRestartNode(t *testing.T) {
	op, node, events := newFakeNoneOperator()

	require.NoError(t, op.RestartNode(t.Context(), discardLogger(), noneActive()))
	assert.Equal(t, []string{"restart"}, *events)

	node.restart = errors.New("kubelet down")

	require.ErrorContains(t, op.RestartNode(t.Context(), discardLogger(), noneActive()), "restart node: kubelet down")
}

// Every daemon start finishes an upgrade the agent did not.
func TestNoneOperatorEnsureLifecycleMigrationRecoversAnInterruptedUpgrade(t *testing.T) {
	op, node, events := newFakeNoneOperator()
	active := noneActive()

	require.NoError(t, op.EnsureLifecycleMigration(t.Context(), discardLogger(), active))
	assert.Equal(t, []string{"recover"}, *events)
	assert.Equal(t, active.Config.Cluster.Version, node.params.Config.Cluster.Version)

	node.recover = errors.New("cannot roll back")

	require.ErrorContains(t, op.EnsureLifecycleMigration(t.Context(), discardLogger(), active), "recover an interrupted node upgrade")
}

func TestNoneOperatorDefaultsAreTheRealNode(t *testing.T) {
	_, ok := NoneOperator{}.node(noneops.Params{Log: discardLogger()}).(*noneops.Node)
	assert.True(t, ok)
}
