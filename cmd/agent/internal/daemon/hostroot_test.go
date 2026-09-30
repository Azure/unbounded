// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

var _ hostRootSteps = hostRootHost{}

type fakeHostRootSteps struct {
	state  hostroot.State
	ready  bool
	calls  []string
	failAt string
}

func (f *fakeHostRootSteps) step(name string) error {
	f.calls = append(f.calls, name)
	if f.failAt == name {
		return errors.New(name + " failed")
	}

	return nil
}

func (f *fakeHostRootSteps) State() (hostroot.State, error) { return f.state, nil }
func (f *fakeHostRootSteps) DiscardStaging() error          { return f.step("discard-staging") }
func (f *fakeHostRootSteps) RecordSelf() error              { return f.step("record-self") }
func (f *fakeHostRootSteps) Ready() (bool, string, error) {
	return f.ready, "not yet", f.step("ready")
}
func (f *fakeHostRootSteps) Stage() error                       { return f.step("stage") }
func (f *fakeHostRootSteps) Swap(context.Context) error         { return f.step("swap") }
func (f *fakeHostRootSteps) RewriteUnits(context.Context) error { return f.step("rewrite-units") }
func (f *fakeHostRootSteps) FinishMove() error                  { return f.step("finish-move") }
func (f *fakeHostRootSteps) RemoveSeed() error                  { return f.step("remove-seed") }
func (f *fakeHostRootSteps) Restart(context.Context) error      { return f.step("restart") }

var (
	moveSteps     = []string{"discard-staging", "record-self", "ready", "stage", "swap", "rewrite-units", "finish-move", "restart"}
	completeSteps = []string{"rewrite-units", "finish-move", "restart"}
)

func TestReconcileHostRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state hostroot.State
		ready bool
		want  []string
	}{
		{name: "installed host removes a seed", state: hostroot.StateInstalled, want: []string{"remove-seed"}},
		// Interrupted after the swap: the copy is in place and has to be
		// finished, whatever the slots now hold.
		{name: "unfinished move is finished", state: hostroot.StateMoving, want: completeSteps},
		{name: "linked host that is not ready stays linked", state: hostroot.StateLinked, want: []string{"discard-staging", "record-self", "ready"}},
		{name: "linked host that is ready is moved", state: hostroot.StateLinked, ready: true, want: moveSteps},
		{name: "no root", state: hostroot.StateAbsent},
		{name: "someone else's root", state: hostroot.StateOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			steps := &fakeHostRootSteps{state: tt.state, ready: tt.ready}
			require.NoError(t, reconcileHostRoot(t.Context(), discardLogger(), steps))
			assert.Equal(t, tt.want, steps.calls)
		})
	}
}

// TestReconcileHostRootStopsAtTheFirstFailure pins that no step runs after one
// fails. In particular the legacy files are never removed before the units are
// rewritten, and nothing is swapped in that was not fully staged.
func TestReconcileHostRootStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()

	for i, failing := range moveSteps {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()

			steps := &fakeHostRootSteps{state: hostroot.StateLinked, ready: true, failAt: failing}
			err := reconcileHostRoot(t.Context(), discardLogger(), steps)

			require.ErrorContains(t, err, failing+" failed")
			assert.Equal(t, moveSteps[:i+1], steps.calls)
		})
	}
}

func TestReconcileHostRootUnderLock(t *testing.T) {
	t.Parallel()

	active := &ActiveMachine{Name: "kube1", Config: &provision.AgentConfig{MachineName: "machine-1"}}
	newStore := func(t *testing.T) *installstate.Store {
		t.Helper()

		return installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	}

	t.Run("runs with ownership", func(t *testing.T) {
		t.Parallel()

		op := &fakeNodeOperator{}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), newStore(t), op, active)

		assert.Equal(t, 1, op.hostRootCalls)
		assert.Same(t, active, op.hostRootActive)
	})

	t.Run("a failure does not stop the daemon", func(t *testing.T) {
		t.Parallel()

		op := &fakeNodeOperator{hostRootErr: errors.New("swap failed")}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), newStore(t), op, active)

		assert.Equal(t, 1, op.hostRootCalls)
	})

	t.Run("skipped while another operation holds the lock", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		lock, err := store.AcquireLock()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, lock.Release()) })

		op := &fakeNodeOperator{}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active)

		assert.Zero(t, op.hostRootCalls)
	})

	t.Run("skipped while an installation is unfinished", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		record, err := installstate.NewRecord("machine-1", "fingerprint")
		require.NoError(t, err)
		require.NoError(t, store.Save(record))

		op := &fakeNodeOperator{}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active)

		assert.Zero(t, op.hostRootCalls)
	})
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))

	return hex.EncodeToString(sum[:])
}

// slotHost lays out a blue-green layout after an upgrade to green.
func slotHost(t *testing.T) (dir string, slots map[string]string) {
	t.Helper()

	dir = t.TempDir()
	for name, content := range map[string]string{"blue": "old agent", "green": "new agent"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755))
	}

	require.NoError(t, os.Symlink(filepath.Join(dir, "green"), filepath.Join(dir, "current")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "blue"), filepath.Join(dir, "last-good")))

	return dir, map[string]string{"current": filepath.Join(dir, "current"), "last-good": filepath.Join(dir, "last-good")}
}

func TestHostRootMoveReady(t *testing.T) {
	t.Parallel()

	t.Run("an older agent in last-good keeps the host linked", func(t *testing.T) {
		t.Parallel()

		dir, slots := slotHost(t)
		agents := filepath.Join(dir, "agents")
		require.NoError(t, recordAgentDigest(agents, digestOf("new agent")))

		ready, reason, err := hostRootMoveReady(agents, filepath.Join(dir, "signal"), slots)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, reason, "last-good")
	})

	t.Run("nothing recorded keeps the host linked", func(t *testing.T) {
		t.Parallel()

		dir, slots := slotHost(t)

		ready, reason, err := hostRootMoveReady(filepath.Join(dir, "agents"), filepath.Join(dir, "signal"), slots)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, reason, "predates the host root")
	})

	t.Run("both slots recorded", func(t *testing.T) {
		t.Parallel()

		dir, slots := slotHost(t)
		agents := filepath.Join(dir, "agents")
		require.NoError(t, recordAgentDigest(agents, digestOf("new agent")))
		require.NoError(t, recordAgentDigest(agents, digestOf("old agent")))

		ready, _, err := hostRootMoveReady(agents, filepath.Join(dir, "signal"), slots)
		require.NoError(t, err)
		assert.True(t, ready)
	})

	t.Run("an unreported upgrade keeps the host linked", func(t *testing.T) {
		t.Parallel()

		dir, slots := slotHost(t)
		agents := filepath.Join(dir, "agents")
		require.NoError(t, recordAgentDigest(agents, digestOf("new agent")))
		require.NoError(t, recordAgentDigest(agents, digestOf("old agent")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "signal"), []byte("{}"), 0o600))

		ready, reason, err := hostRootMoveReady(agents, filepath.Join(dir, "signal"), slots)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, reason, "not been reported")
	})

	t.Run("a slot that does not resolve keeps the host linked", func(t *testing.T) {
		t.Parallel()

		dir, slots := slotHost(t)
		agents := filepath.Join(dir, "agents")
		require.NoError(t, recordAgentDigest(agents, digestOf("new agent")))
		require.NoError(t, os.Remove(filepath.Join(dir, "blue")))

		ready, reason, err := hostRootMoveReady(agents, filepath.Join(dir, "signal"), slots)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, reason, "does not resolve")
	})
}

func TestRecordAgentDigest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agents")
	first, second := digestOf("b"), digestOf("a")

	require.NoError(t, recordAgentDigest(path, first))
	require.NoError(t, recordAgentDigest(path, first))
	require.NoError(t, recordAgentDigest(path, second))

	known, err := loadAgentDigests(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{first: true, second: true}, known)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(data)), 2, "a digest is recorded once")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestLoadAgentDigestsIgnoresWhatIsNotADigest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agents")
	valid := digestOf("agent")
	require.NoError(t, os.WriteFile(path, []byte("not-a-digest\n"+valid+"\nabcd\n"), 0o600))

	known, err := loadAgentDigests(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{valid: true}, known)

	known, err = loadAgentDigests(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	assert.Empty(t, known, "a host that has recorded nothing knows no agents")
}

func TestHostRootHostRecordsItsOwnBinary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	self := filepath.Join(dir, "unbounded-agent-green")
	require.NoError(t, os.WriteFile(self, []byte("new agent"), 0o755))

	h := hostRootHost{
		log:        discardLogger(),
		agents:     filepath.Join(dir, "agents"),
		executable: func() (string, error) { return self, nil },
	}
	require.NoError(t, h.RecordSelf())

	known, err := loadAgentDigests(h.agents)
	require.NoError(t, err)
	assert.True(t, known[digestOf("new agent")])
}

// TestHostRootHostIsNotReadyWithABinaryOverride covers a host whose daemon
// binary paths are moved away from the layout. The move copies the layout, so
// it cannot carry them.
func TestHostRootHostIsNotReadyWithABinaryOverride(t *testing.T) {
	t.Parallel()

	for _, name := range daemonBinaryOverrides {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := hostRootHost{
				log:    discardLogger(),
				agents: filepath.Join(t.TempDir(), "agents"),
				lookupEnv: func(key string) (string, bool) {
					if key == name {
						return "/srv/agent", true
					}

					return "", false
				},
			}

			ready, reason, err := h.Ready()
			require.NoError(t, err)
			assert.False(t, ready)
			assert.Contains(t, reason, name)
		})
	}

	assert.Contains(t, daemonBinaryOverrides, goalstates.EnvDaemonBinaryLastGood)
}
