// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// stubNode is a hostNode that records what the shared sequence asks of it.
type stubNode struct {
	running                        bool
	runningErr, configureErr       error
	startErr                       error
	containerd, kubelet            bool
	started                        bool
	startedWas, startedC, startedK bool
}

func (n *stubNode) Running(context.Context) (bool, error) { return n.running, n.runningErr }

func (n *stubNode) Configure(context.Context) (bool, bool, error) {
	return n.containerd, n.kubelet, n.configureErr
}

func (n *stubNode) Start(_ context.Context, was, c, k bool) error {
	n.started, n.startedWas, n.startedC, n.startedK = true, was, c, k

	return n.startErr
}

type seamLog struct {
	events []string
}

func (l *seamLog) seams() hostSeams {
	return hostSeams{
		sync: func(paths ...string) error {
			l.events = append(l.events, "sync "+paths[0])
			return nil
		},
		archives: func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task {
			return hostnode.Func("archives", func(context.Context) error { l.events = append(l.events, "archives"); return nil })
		},
		persist: func(_ *slog.Logger, slot string, _ *config.AgentConfig) phases.Task {
			return hostnode.Func("persist", func(context.Context) error { l.events = append(l.events, "persist "+slot); return nil })
		},
	}
}

var testMode = hostMode{
	mode: isolation.ModeNone, slot: "kube1", base: "/base",
	prepareTask: "build-node", configureTask: "configure-node", startTask: "start-node",
	runningMsg: "already running",
}

func hostStages() *agentStages {
	return &agentStages{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:              &provision.UnboundedAgentConfig{},
		credentialsReady: true,
	}
}

func TestHostModePrepareNodeRootBuildsAfterArchivesAndSyncsTheBase(t *testing.T) {
	l := &seamLog{}
	built := false

	err := testMode.prepareNodeRoot(t.Context(), hostStages(), &stubNode{}, l.seams(), func(context.Context) error {
		l.events = append(l.events, "build")
		built = true

		return nil
	})
	require.NoError(t, err)
	assert.True(t, built)
	assert.Equal(t, []string{"archives", "build", "sync /base"}, l.events)
}

func TestHostModePrepareNodeRootLeavesARunningNodeAlone(t *testing.T) {
	l := &seamLog{}

	err := testMode.prepareNodeRoot(t.Context(), hostStages(), &stubNode{running: true}, l.seams(), func(context.Context) error {
		t.Fatal("a running node is not rebuilt")
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, l.events)
}

func TestHostModePrepareNodeRootNamesTheFailedTask(t *testing.T) {
	l := &seamLog{}

	err := testMode.prepareNodeRoot(t.Context(), hostStages(), &stubNode{}, l.seams(), func(context.Context) error {
		return errors.New("disk full")
	})
	require.ErrorContains(t, err, "build-node")
	require.ErrorContains(t, err, "disk full")
	assert.NotContains(t, l.events, "sync /base", "a failed build is not synced")
}

func TestHostModePrepareNodeRootReportsAStatusError(t *testing.T) {
	boom := errors.New("status failed")

	err := testMode.prepareNodeRoot(t.Context(), hostStages(), &stubNode{runningErr: boom}, (&seamLog{}).seams(), func(context.Context) error { return nil })
	require.ErrorIs(t, err, boom)
}

func TestHostModeEnsureNodeStartedRecordsOnlyANodeItBuilt(t *testing.T) {
	l := &seamLog{}
	n := &stubNode{containerd: true}

	require.NoError(t, testMode.ensureNodeStarted(t.Context(), hostStages(), n, l.seams()))
	assert.True(t, n.started)
	assert.False(t, n.startedWas)
	assert.True(t, n.startedC, "what Configure changed reaches Start")
	assert.False(t, n.startedK)
	assert.Equal(t, []string{"persist kube1", "sync /base"}, l.events)

	l = &seamLog{}
	n = &stubNode{running: true, kubelet: true}

	require.NoError(t, testMode.ensureNodeStarted(t.Context(), hostStages(), n, l.seams()))
	assert.True(t, n.startedWas)
	assert.True(t, n.startedK)
	assert.Equal(t, []string{"sync /base"}, l.events, "a node found running was not built by this attempt")
}

func TestHostModeEnsureNodeStartedNamesTheFailedTaskAndRecordsNothing(t *testing.T) {
	l := &seamLog{}

	err := testMode.ensureNodeStarted(t.Context(), hostStages(), &stubNode{configureErr: errors.New("bad config")}, l.seams())
	require.ErrorContains(t, err, "configure-node")
	assert.Empty(t, l.events)

	err = testMode.ensureNodeStarted(t.Context(), hostStages(), &stubNode{startErr: errors.New("exec format error")}, l.seams())
	require.ErrorContains(t, err, "start-node")
	require.ErrorContains(t, err, "exec format error")
	assert.Empty(t, l.events, "a node that never started is not recorded as built")
}

func TestHostSeamsDefaultsAreTheRealHostSteps(t *testing.T) {
	t.Parallel()

	var h hostSeams

	require.NotNil(t, h.downloadArchives(hostStages()))
	require.NotNil(t, h.persistApplied(slog.New(slog.DiscardHandler), "kube1", &config.AgentConfig{}))
	require.NoError(t, h.syncFS(), "syncing nothing succeeds")
}
