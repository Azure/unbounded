// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stubNodeSeams replaces the shared resolve and stage seams with recorders that
// append "resolve" and "stage" to events, and restores all four shared seams
// when the test ends. The caller replaces the persist and remove seams it
// asserts on.
func stubNodeSeams(t *testing.T, events *[]string) {
	t.Helper()

	oldResolve, oldStage, oldPersist, oldRemove := resolveNodeDownloads, stageNodeArchives, persistNodeConfig, removeNodeConfig

	t.Cleanup(func() {
		resolveNodeDownloads, stageNodeArchives, persistNodeConfig, removeNodeConfig = oldResolve, oldStage, oldPersist, oldRemove
	})

	resolveNodeDownloads = func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		*events = append(*events, "resolve")
		return &goalstates.DownloadOverrides{}, &goalstates.ContainerImageArchiveStaging{HostDir: "/x"}, nil
	}
	stageNodeArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		*events = append(*events, "stage")
		return nil
	}
}

func TestStubNodeSeamsRestoresTheSharedSeams(t *testing.T) {
	before := fmt.Sprintf("%p %p %p %p", resolveNodeDownloads, stageNodeArchives, persistNodeConfig, removeNodeConfig)

	t.Run("inner", func(t *testing.T) {
		events := &[]string{}
		stubNodeSeams(t, events)

		_, _, err := resolveNodeDownloads(t.Context(), nil)
		require.NoError(t, err)
		require.NoError(t, stageNodeArchives(t.Context(), discardLogger(), nil))
		assert.Equal(t, []string{"resolve", "stage"}, *events)
	})

	assert.Equal(t, before, fmt.Sprintf("%p %p %p %p", resolveNodeDownloads, stageNodeArchives, persistNodeConfig, removeNodeConfig))
}

func TestRestartActiveNodeRunsTheRestartAndWrapsFailures(t *testing.T) {
	active := &ActiveMachine{Name: "kube2", Mode: isolation.ModeContainer}

	var ran int

	require.NoError(t, restartActiveNode(t.Context(), discardLogger(), active, func(context.Context) error { ran++; return nil }))
	assert.Equal(t, 1, ran)

	boom := errors.New("kubelet not active")
	err := restartActiveNode(t.Context(), discardLogger(), active, func(context.Context) error { return boom })
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "restart node")
}
