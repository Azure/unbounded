// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeNSpawn, false},
		{"nspawn", ModeNSpawn, false},
		{"none", ModeNone, false},
		{"container", ModeContainer, false},
		{"microvm", "", true},
		{"NSpawn", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseMode(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}

			if got != tt.want {
				t.Fatalf("ParseMode(%q) = %q, want %q", tt.in, got, tt.want)
			}

			if tt.wantErr && !strings.Contains(err.Error(), tt.in) {
				t.Fatalf("error %q should name the rejected value", err)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	const mode Mode = "none"

	t.Cleanup(func() { Unregister(mode) })

	if _, err := New(mode, nil); err == nil {
		t.Fatal("New with no registered backend should fail")
	}

	var gotDeps any

	Register(mode, func(deps any) (Backend, error) {
		gotDeps = deps
		return &FakeBackend{FakeMode: mode}, nil
	})

	b, err := New(mode, "deps")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if b.Mode() != mode || gotDeps != "deps" {
		t.Fatalf("mode = %q, deps = %v", b.Mode(), gotDeps)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register should panic")
		}
	}()

	Register(mode, func(any) (Backend, error) { return nil, nil })
}

// A nil factory would be found at the first New, far from the Register call
// that is wrong.
func TestRegisterRejectsANilFactory(t *testing.T) {
	require.Panics(t, func() { Register("container", nil) })
	require.ErrorContains(t, Registered("container"), "no registered backend", "nothing was registered")
}

// A factory that returns neither a backend nor an error would make New hand
// the caller a nil Backend to dereference.
func TestNewRejectsAFactoryThatBuildsNothing(t *testing.T) {
	const mode Mode = "container"

	t.Cleanup(func() { Unregister(mode) })
	Register(mode, func(any) (Backend, error) { return nil, nil })

	b, err := New(mode, nil)
	require.ErrorContains(t, err, "no backend")
	require.Nil(t, b)
}

func TestNewRejectsUnknownMode(t *testing.T) {
	if _, err := New("bogus", nil); err == nil {
		t.Fatal("unknown mode should fail")
	}
}

func TestNewEmptyModeIsDefault(t *testing.T) {
	t.Cleanup(func() { Unregister(DefaultMode) })
	Register(DefaultMode, func(any) (Backend, error) { return &FakeBackend{}, nil })

	b, err := New("", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if b.Mode() != DefaultMode {
		t.Fatalf("mode = %q, want %q", b.Mode(), DefaultMode)
	}
}

func TestDirRootPath(t *testing.T) {
	if got := (DirRoot{SlotName: "kube1"}).Path("/etc/x"); got != "/etc/x" {
		t.Fatalf("identity path = %q", got)
	}

	r := DirRoot{SlotName: "kube1", Dir: "/var/lib/machines/kube1"}
	if got := r.Path("/etc/x"); got != "/var/lib/machines/kube1/etc/x" {
		t.Fatalf("path = %q", got)
	}

	if r.Slot() != "kube1" {
		t.Fatalf("slot = %q", r.Slot())
	}
}

func TestFakeBackendRecordsAndInjects(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	f := &FakeBackend{Dir: "/r", StartErr: boom}

	root, err := f.Prepare(ctx, NodeSpec{Slot: "kube2"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := f.Start(ctx, root); !errors.Is(err, boom) {
		t.Fatalf("Start error = %v", err)
	}

	if _, err := f.Exec(ctx, root, ExecSpec{Args: []string{"true"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if err := f.Stop(ctx, root); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	want := []string{"prepare kube2", "start kube2", "exec kube2", "stop kube2"}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	if got := f.Execs(); len(got) != 1 || got[0].Args[0] != "true" {
		t.Fatalf("execs = %v", got)
	}
}

func TestFakeBackendPrepareError(t *testing.T) {
	f := &FakeBackend{PrepareErr: errors.New("nope")}
	if _, err := f.Prepare(context.Background(), NodeSpec{Slot: "kube1"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestUnregisterReturnsTheFactoryForRestoring(t *testing.T) {
	const mode Mode = "container"

	Register(mode, func(any) (Backend, error) { return &FakeBackend{FakeMode: mode}, nil })

	f, ok := Unregister(mode)
	require.True(t, ok)

	_, err := New(mode, nil)
	require.ErrorContains(t, err, "no registered backend")

	Register(mode, f)

	b, err := New(mode, nil)
	require.NoError(t, err)
	require.Equal(t, mode, b.Mode())

	Unregister(mode)

	_, ok = Unregister(mode)
	require.False(t, ok)
}

func TestRegisteredChecksWithoutBuildingABackend(t *testing.T) {
	const mode Mode = "none"

	built := 0

	Register(mode, func(any) (Backend, error) {
		built++

		return &FakeBackend{FakeMode: mode}, nil
	})

	t.Cleanup(func() { Unregister(mode) })

	require.NoError(t, Registered(mode))
	require.Zero(t, built, "checking a mode builds nothing")

	require.ErrorContains(t, Registered("microvm"), "unsupported isolation mode")

	f, ok := Unregister(mode)
	require.True(t, ok)

	require.ErrorContains(t, Registered(mode), "no registered backend")

	Register(mode, f)
}
