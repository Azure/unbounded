// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

type fakeHostRootSteps struct {
	state hostroot.State
	ready bool
	calls []string
}

func (f *fakeHostRootSteps) step(name string) error {
	f.calls = append(f.calls, name)

	return nil
}

func (f *fakeHostRootSteps) State() (hostroot.State, error) { return f.state, nil }
func (f *fakeHostRootSteps) RecordSelf() error              { return f.step("record-self") }
func (f *fakeHostRootSteps) Ready() (bool, string, error) {
	return f.ready, "not yet", f.step("ready")
}
func (f *fakeHostRootSteps) Move(context.Context) error         { return f.step("move") }
func (f *fakeHostRootSteps) RewriteUnits(context.Context) error { return f.step("rewrite-units") }
func (f *fakeHostRootSteps) FinishMove() error                  { return f.step("finish-move") }
func (f *fakeHostRootSteps) RemoveSeed() error                  { return f.step("remove-seed") }
func (f *fakeHostRootSteps) Restart(context.Context) error      { return f.step("restart") }

var completeSteps = []string{"rewrite-units", "finish-move", "restart"}

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
		{name: "linked host that is not ready stays linked", state: hostroot.StateLinked, want: []string{"record-self", "ready"}},
		{
			name: "linked host that is ready is moved", state: hostroot.StateLinked, ready: true,
			want: append([]string{"record-self", "ready", "move"}, completeSteps...),
		},
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
func slotHost(t *testing.T) (dir, current, lastGood string) {
	t.Helper()

	dir = t.TempDir()
	for name, content := range map[string]string{"blue": "old agent", "green": "new agent"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755))
	}

	current, lastGood = filepath.Join(dir, "current"), filepath.Join(dir, "last-good")
	require.NoError(t, os.Symlink(filepath.Join(dir, "green"), current))
	require.NoError(t, os.Symlink(filepath.Join(dir, "blue"), lastGood))

	return dir, current, lastGood
}

func TestHostRootMoveReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recorded   []string
		setup      func(t *testing.T, dir string)
		wantReason string
	}{
		{name: "an older agent in last-good keeps the host linked", recorded: []string{"new agent"}, wantReason: "last-good binary"},
		{name: "both slots recorded", recorded: []string{"new agent", "old agent"}},
		{
			name:     "an unreported upgrade keeps the host linked",
			recorded: []string{"new agent", "old agent"},
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "signal"), []byte("{}"), 0o600))
			},
			wantReason: "not been reported",
		},
		{
			name:       "a slot that does not resolve keeps the host linked",
			recorded:   []string{"new agent"},
			setup:      func(t *testing.T, dir string) { require.NoError(t, os.Remove(filepath.Join(dir, "blue"))) },
			wantReason: "does not resolve",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir, current, lastGood := slotHost(t)
			agents := filepath.Join(dir, "agents")

			for _, content := range tt.recorded {
				require.NoError(t, recordAgentDigest(agents, digestOf(content)))
			}

			if tt.setup != nil {
				tt.setup(t, dir)
			}

			ready, reason, err := hostRootMoveReady(agents, filepath.Join(dir, "signal"), current, lastGood)
			require.NoError(t, err)
			assert.Equal(t, tt.wantReason == "", ready)
			assert.Contains(t, reason, tt.wantReason)
		})
	}
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
