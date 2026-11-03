// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stubResetHost replaces what detectResetMode reads and returns how many times
// the applied config was read.
func stubResetHost(t *testing.T, applied isolation.Mode, readErr error, noneTraces, containerTraces bool) *int {
	t.Helper()

	oldRead, oldNone, oldContainer := readAppliedMode, hostIsNone, hostIsContainer

	t.Cleanup(func() { readAppliedMode, hostIsNone, hostIsContainer = oldRead, oldNone, oldContainer })

	reads := 0
	readAppliedMode = func() (isolation.Mode, error) {
		reads++

		return applied, readErr
	}
	hostIsNone = func() bool { return noneTraces }
	hostIsContainer = func() bool { return containerTraces }

	return &reads
}

func TestDetectResetMode(t *testing.T) {
	unreadable := errors.New("checksum mismatch")

	tests := []struct {
		name            string
		applied         isolation.Mode
		readErr         error
		noneTraces      bool
		containerTraces bool
		want            isolation.Mode
	}{
		{name: "readable nspawn ignores stray none traces", applied: isolation.ModeNSpawn, noneTraces: true, want: isolation.ModeNSpawn},
		{name: "readable nspawn ignores stray container traces", applied: isolation.ModeNSpawn, containerTraces: true, want: isolation.ModeNSpawn},
		{name: "readable nspawn ignores both", applied: isolation.ModeNSpawn, noneTraces: true, containerTraces: true, want: isolation.ModeNSpawn},
		{name: "readable none", applied: isolation.ModeNone, want: isolation.ModeNone},
		{name: "readable none ignores container traces", applied: isolation.ModeNone, containerTraces: true, want: isolation.ModeNone},
		{name: "readable container", applied: isolation.ModeContainer, want: isolation.ModeContainer},
		{name: "readable container ignores none traces", applied: isolation.ModeContainer, noneTraces: true, want: isolation.ModeContainer},
		{name: "gone with none traces", readErr: appliedconfig.ErrNotFound, noneTraces: true, want: isolation.ModeNone},
		{name: "gone with container traces", readErr: appliedconfig.ErrNotFound, containerTraces: true, want: isolation.ModeContainer},
		// A config that exists but cannot be read says nothing about the mode, and
		// a leftover of another mode is no evidence of it either, so every mode's
		// teardown runs.
		{name: "unreadable with none traces", readErr: unreadable, noneTraces: true, want: resetUnknown},
		{name: "unreadable with container traces", readErr: unreadable, containerTraces: true, want: resetUnknown},
		{name: "unreadable with both traces", readErr: unreadable, noneTraces: true, containerTraces: true, want: resetUnknown},
		{name: "gone and nothing else is nspawn", readErr: appliedconfig.ErrNotFound, want: isolation.ModeNSpawn},
		{name: "unreadable and nothing else", readErr: unreadable, want: resetUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := stubResetHost(t, tt.applied, tt.readErr, tt.noneTraces, tt.containerTraces)

			assert.Equal(t, tt.want, detectResetMode(discardLogger()))
			assert.Equal(t, 1, *reads, "the applied config is read once")
		})
	}
}

// The scenario the union is for: an nspawn host whose applied config is
// corrupt and that has a stray none directory. Guessing none would leave the
// machines running.
func TestAnUnreadableAppliedConfigTearsDownEveryMode(t *testing.T) {
	stubResetHost(t, "", errors.New("checksum mismatch"), true, false)

	name := resetNodeResources(discardLogger()).Name()

	for _, step := range []string{
		"parallel(stop-machine, stop-machine)", "stop-none-node", "stop-container-node",
		"parallel(remove-nspawn-config, remove-nspawn-config)", "remove-none-config", "remove-container-config",
		"parallel(remove-machine, remove-machine)", "remove-none-node", "remove-container-node",
		"parallel(remove-bpffs-mount, remove-bpffs-mount)",
	} {
		assert.Contains(t, name, step)
	}

	// The same order as every single-mode reset: stop, then config, then the
	// node, around the host-global cleanup.
	last := -1

	for _, step := range []string{
		"remove-daemon-unit", "stop-machine", "stop-none-node", "stop-container-node", "remove-wireguard-keys",
		"remove-nspawn-config", "remove-none-config", "remove-container-config",
		"remove-machine", "remove-none-node", "remove-container-node", "remove-bpffs-mount",
		"cleanup-routes", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	} {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}
}

func TestAConfigThatIsGoneStillFollowsTheTraces(t *testing.T) {
	stubResetHost(t, "", appliedconfig.ErrNotFound, true, false)

	name := resetNodeResources(discardLogger()).Name()
	assert.Contains(t, name, "stop-none-node")
	assert.NotContains(t, name, "stop-machine", "a reset that is retried after the config went is not a reset of everything")
}

func TestResetNodeResourcesFollowTheAppliedModeNotTheTraces(t *testing.T) {
	stubResetHost(t, isolation.ModeNSpawn, nil, true, true)

	name := resetNodeResources(discardLogger()).Name()
	assert.Contains(t, name, "stop-machine", "an nspawn host with leftovers is torn down as nspawn")
	assert.NotContains(t, name, "stop-none-node")

	stubResetHost(t, isolation.ModeNone, nil, false, false)
	assert.Contains(t, resetNodeResources(discardLogger()).Name(), "stop-none-node")

	stubResetHost(t, isolation.ModeContainer, nil, false, false)
	assert.NotContains(t, resetNodeResources(discardLogger()).Name(), "stop-machine")
	assert.NotContains(t, resetNodeResources(discardLogger()).Name(), "stop-none-node")
}

func TestResetBuildsItsTasksFromOneRead(t *testing.T) {
	reads := stubResetHost(t, isolation.ModeNone, nil, false, false)

	_ = ResetAgent(discardLogger())

	assert.Equal(t, 1, *reads, "ResetAgent decides the mode once for the teardown and the sync paths")

	*reads = 0
	_ = ResetAgentForReprovision(discardLogger())

	assert.Equal(t, 1, *reads)
}
