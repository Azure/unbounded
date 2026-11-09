// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

// machineState stands in for the host: the lifecycle Ops flip it, and the
// systemd-run stub on PATH reads it, so Status and Exec see what Start did.
type machineState struct {
	mu       sync.Mutex
	dir      string
	prepared bool
	fail     map[isolationtest.Op]error
}

func (m *machineState) runningFile() string { return filepath.Join(m.dir, "running") }

func (m *machineState) take(op isolationtest.Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.fail[op]
	delete(m.fail, op)

	return err
}

func (m *machineState) setRunning(on bool) error {
	if !on {
		if err := os.Remove(m.runningFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	}

	return os.WriteFile(m.runningFile(), nil, 0o644)
}

func (m *machineState) ops() Ops {
	return Ops{
		Prepare: func(_ context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
			if err := m.take(isolationtest.OpPrepare); err != nil {
				return nil, err
			}

			m.mu.Lock()
			m.prepared = true
			m.mu.Unlock()

			return Root(n.Slot), nil
		},
		Start: func(_ context.Context, _ isolation.SlotRef) error {
			if err := m.take(isolationtest.OpStart); err != nil {
				return err
			}

			m.mu.Lock()
			defer m.mu.Unlock()

			if !m.prepared {
				return errors.New("machine not prepared")
			}

			return m.setRunning(true)
		},
		Stop: func(_ context.Context, _ isolation.SlotRef) error {
			if err := m.take(isolationtest.OpStop); err != nil {
				return err
			}

			return m.setRunning(false)
		},
		Restart: func(_ context.Context, _ isolation.SlotRef) error {
			if err := m.take(isolationtest.OpRestart); err != nil {
				return err
			}

			return m.setRunning(true)
		},
		Destroy: func(_ context.Context, _ isolation.SlotRef) error {
			m.mu.Lock()
			m.prepared = false
			m.mu.Unlock()

			return m.setRunning(false)
		},
	}
}

// TestConformance runs the shared suite with host commands stubbed on PATH.
// systemd-run succeeds only while the machine state says running, which is how
// a real machine rejects a command sent to a machine that is not up.
func TestConformance(t *testing.T) {
	isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
		m := &machineState{dir: t.TempDir(), fail: map[isolationtest.Op]error{}}

		bin := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bin, "systemd-run"), []byte(`#!/bin/sh
[ -e "`+m.runningFile()+`" ] || { echo "Failed to connect to machine" >&2; exit 1; }
shift 3
exec "$@"
`), 0o755))
		// The stub echoes its arguments, so the suite can check which journal the
		// backend asked for.
		require.NoError(t, os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\necho \"journalctl $*\"\n"), 0o755))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		return &Backend{Ops: m.ops()}, isolationtest.Options{
			Spec:         isolation.NodeSpec{Slot: "kube1", Version: "v1.34.0"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			// systemd-run fails while the machine is not up.
			ExecStoppedFails: true,
			LogsWant:         "journalctl --machine=kube1 --no-pager --lines=10",
			// nspawn derives its devices, mounts and libraries from the host when
			// the machine is built, so a request for one must fail, not be dropped.
			RefusedGrants: []isolation.Grant{
				{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/kvm", Target: "/dev/kvm"},
				{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data", Target: "/data"},
			},
			Fail: func(op isolationtest.Op, err error) {
				m.mu.Lock()
				m.fail[op] = err
				m.mu.Unlock()
			},
		}
	})
}

func TestUnwiredBackendFailsConformanceOperations(t *testing.T) {
	b := &Backend{}

	_, err := b.Prepare(t.Context(), isolation.NodeSpec{Slot: "kube1"})
	require.ErrorIs(t, err, ErrNotWired)
	require.ErrorIs(t, b.Start(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Stop(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Restart(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Destroy(t.Context(), Root("kube1")), ErrNotWired)
}

// Exec and Logs run the commands the rest of the agent depends on: a command in
// the machine for the slot, and the machine's journal. The conformance suite
// checks that they return what the stubs print; this checks what they asked for.
func TestExecAndLogsAskForTheMachineOfTheSlot(t *testing.T) {
	bin := t.TempDir()
	record := filepath.Join(bin, "args")

	for _, name := range []string{"systemd-run", "journalctl"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(
			"#!/bin/sh\necho \""+name+" $*\" >> "+record+"\n"), 0o755))
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := &Backend{}

	_, err := b.Exec(t.Context(), Root("kube2"), isolation.ExecSpec{Args: []string{"crictl", "ps", "-a"}})
	require.NoError(t, err)

	rc, err := b.Logs(t.Context(), Root("kube2"), isolation.LogOptions{Unit: "kubelet", Follow: true, Lines: 5})
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	got, err := os.ReadFile(record)
	require.NoError(t, err)
	require.Equal(t,
		"systemd-run --machine=kube2 --pipe --wait crictl ps -a\n"+
			"journalctl --machine=kube2 --no-pager --unit=kubelet --follow --lines=5\n",
		string(got))
}

func TestPrepareRefusesGrantsBecauseTheMachineDerivesItsOwn(t *testing.T) {
	prepared := false
	b := &Backend{Ops: Ops{Prepare: func(context.Context, isolation.NodeSpec) (isolation.NodeRoot, error) {
		prepared = true
		return Root("kube1"), nil
	}}}

	_, err := b.Prepare(t.Context(), isolation.NodeSpec{Slot: "kube1", Grants: []isolation.Grant{
		{Kind: isolation.GrantDevice, Source: "/dev/kvm", Target: "/dev/kvm"},
	}})
	require.ErrorContains(t, err, "grants")
	require.False(t, prepared, "nothing is built for a request that is dropped")

	_, err = b.Prepare(t.Context(), isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)
	require.True(t, prepared)
}

// A canceled probe says nothing about the machine: it must not read as a node
// that is down.
func TestStatusOfACanceledProbeIsAnErrorNotAStoppedNode(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	st, err := (&Backend{}).Status(ctx, Root("kube1"))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, st.Running)
}

func TestStatusOfAMachineThatIsNotUpIsAStatusNotAnError(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\necho 'Failed to connect to machine' >&2\nexit 1\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, err := (&Backend{}).Status(t.Context(), Root("kube1"))
	require.NoError(t, err)
	require.False(t, st.Running)
	require.NotEmpty(t, st.Detail)
}
