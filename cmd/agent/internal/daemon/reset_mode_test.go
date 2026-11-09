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

// stubTeardown names its tasks after its mode, as the real teardowns do.
type stubTeardown struct{ mode string }

func (s stubTeardown) Stop() phases.Task {
	return hostnode.Func("stop-"+s.mode+"-node", func(context.Context) error { return nil })
}

func (s stubTeardown) RemoveConfig() phases.Task {
	return hostnode.Func("remove-"+s.mode+"-config", func(context.Context) error { return nil })
}

func (s stubTeardown) Destroy() phases.Task {
	return hostnode.Func("remove-"+s.mode+"-node", func(context.Context) error { return nil })
}

func stubResetHost(nonePresent bool) (resetHost, *int) {
	return stubResetHostWith(nonePresent, false)
}

func stubResetHostWith(nonePresent, containerPresent bool) (resetHost, *int) {
	asked := 0

	return resetHost{
		nonePresent:       func() bool { asked++; return nonePresent },
		noneTeardown:      func(*slog.Logger) hostNodeTeardown { return stubTeardown{mode: "none"} },
		containerPresent:  func() bool { return containerPresent },
		containerTeardown: func(*slog.Logger) hostNodeTeardown { return stubTeardown{mode: "container"} },
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
		"cleanup-routes", "remove-nftables-flush", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	} {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}
}

// A host with no trace of a container node gets no container teardown: it
// never removes the shared kubelet, CNI or image directories of a host that
// never ran one.
func TestResetRunsTheContainerTeardownOnlyWhereAContainerNodeLeftTraces(t *testing.T) {
	h, _ := stubResetHostWith(false, false)
	name := resetResourcesOn(discardLogger(), h).Name()

	for _, step := range []string{"stop-container-node", "remove-container-config", "remove-container-node"} {
		assert.NotContains(t, name, step)
	}

	h, _ = stubResetHostWith(false, true)
	name = resetResourcesOn(discardLogger(), h).Name()

	for _, step := range nspawnTeardownSteps {
		assert.Contains(t, name, step, "the nspawn teardown still runs")
	}

	last := -1

	for _, step := range []string{
		"remove-daemon-unit", "stop-machine", "stop-container-node", "remove-wireguard-keys",
		"remove-nspawn-config", "remove-container-config",
		"remove-machine", "remove-container-node", "remove-bpffs-mount",
		"cleanup-routes", "remove-nftables-flush", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	} {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}

	assert.NotContains(t, name, "none-node")
}

// The container bootstrap installs the nftables flush unit wanted by
// multi-user.target, so a reset of a container host must remove it, or the
// reset host keeps flushing its own firewall at every boot. Its removal runs
// once, after the container teardown and before systemd is reloaded.
func TestResetOfAContainerHostRemovesTheNFTablesFlush(t *testing.T) {
	h, _ := stubResetHostWith(false, true)
	name := resetResourcesOn(discardLogger(), h).Name()

	require.Equal(t, 1, strings.Count(name, "remove-nftables-flush"), name)
	assert.Less(t, strings.Index(name, "remove-container-node"), strings.Index(name, "remove-nftables-flush"))
	assert.Less(t, strings.Index(name, "remove-nftables-flush"), strings.Index(name, "reload-systemd"))
}

// A host with traces of both (for example a change between them that was
// interrupted) gets both teardowns, none first, in each position.
func TestResetRunsEveryPresentModesTeardown(t *testing.T) {
	h, _ := stubResetHostWith(true, true)
	name := resetResourcesOn(discardLogger(), h).Name()

	for _, pair := range [][2]string{
		{"stop-none-node", "stop-container-node"},
		{"remove-none-config", "remove-container-config"},
		{"remove-none-node", "remove-container-node"},
	} {
		first, second := strings.Index(name, pair[0]), strings.Index(name, pair[1])
		require.GreaterOrEqual(t, first, 0, pair[0])
		require.Greater(t, second, first, "%s before %s", pair[0], pair[1])
	}
}

// The production host inspection is each mode's own traces.
func TestDefaultResetHostLooksForEachModesTraces(t *testing.T) {
	h := defaultResetHost()
	require.NotNil(t, h.nonePresent)
	require.NotNil(t, h.noneTeardown(discardLogger()))
	require.NotNil(t, h.containerPresent)
	require.NotNil(t, h.containerTeardown(discardLogger()))
}
