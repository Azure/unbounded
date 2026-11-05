// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type stubTeardown struct{}

func (stubTeardown) Stop() phases.Task {
	return hostnode.Func("stop-none-node", func(context.Context) error { return nil })
}

func (stubTeardown) RemoveConfig() phases.Task {
	return hostnode.Func("remove-none-config", func(context.Context) error { return nil })
}

func (stubTeardown) Destroy() phases.Task {
	return hostnode.Func("remove-none-node", func(context.Context) error { return nil })
}

func stubResetHost(nonePresent bool) (resetHost, *int) {
	asked := 0

	return resetHost{
		nonePresent:  func() bool { asked++; return nonePresent },
		noneTeardown: func(*slog.Logger) hostNodeTeardown { return stubTeardown{} },
	}, &asked
}

var nspawnTeardownSteps = []string{
	"parallel(stop-machine, stop-machine)",
	"parallel(remove-nspawn-config, remove-nspawn-config)",
	"parallel(remove-machine, remove-machine)",
	"parallel(remove-bpffs-mount, remove-bpffs-mount)",
}

// The nspawn teardown is idempotent and always runs, whatever the host looks
// like: a damaged or missing applied config, or a stray directory of another
// mode, never causes the nspawn machines to be left running.
func TestResetAlwaysRunsTheNSpawnTeardown(t *testing.T) {
	for _, present := range []bool{false, true} {
		h, _ := stubResetHost(present)
		name := resetResourcesOn(discardLogger(), h).Name()

		for _, step := range nspawnTeardownSteps {
			assert.Contains(t, name, step, "none traces present: %v", present)
		}
	}
}

// A host with no trace of a none node gets no none teardown: it never deletes
// another installation's containerd or kubelet files, kills its shims or its
// pods.
func TestResetRunsTheNoneTeardownOnlyWhereANoneNodeLeftTraces(t *testing.T) {
	h, asked := stubResetHost(false)
	name := resetResourcesOn(discardLogger(), h).Name()

	for _, step := range []string{"stop-none-node", "remove-none-config", "remove-none-node"} {
		assert.NotContains(t, name, step)
	}

	assert.Equal(t, 1, *asked, "the host is inspected once per reset")

	h, _ = stubResetHost(true)
	name = resetResourcesOn(discardLogger(), h).Name()

	for _, step := range []string{"stop-none-node", "remove-none-config", "remove-none-node"} {
		assert.Contains(t, name, step)
	}
}

// Each mode's tasks sit in the position of the nspawn task they correspond to,
// around the host-global cleanup.
func TestResetOrderWithANoneNode(t *testing.T) {
	h, _ := stubResetHost(true)
	name := resetResourcesOn(discardLogger(), h).Name()

	last := -1

	for _, step := range []string{
		"remove-daemon-unit", "stop-machine", "stop-none-node", "remove-wireguard-keys",
		"remove-nspawn-config", "remove-none-config",
		"remove-machine", "remove-none-node", "remove-bpffs-mount",
		"cleanup-routes", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	} {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}
}

// The production host inspection is the none node's own traces.
func TestDefaultResetHostLooksForNoneTraces(t *testing.T) {
	h := defaultResetHost()
	require.NotNil(t, h.nonePresent)
	require.NotNil(t, h.noneTeardown(discardLogger()))
}
