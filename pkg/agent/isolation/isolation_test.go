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
		{"container", "", true},
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

func TestDirRootPath(t *testing.T) {
	r := DirRoot{SlotName: "kube1", Dir: "/var/lib/machines/kube1"}
	require.Equal(t, "/var/lib/machines/kube1/etc/x", r.Path("/etc/x"))
	require.Equal(t, "/var/lib/machines/kube1/etc/x", r.Path("etc/x"))
	require.Equal(t, "/var/lib/machines/kube1", r.Path("/"))
	require.Equal(t, "kube1", r.Slot())
}

// An empty Dir used to be the identity mapping, so a DirRoot built from an
// unset machine directory placed the node's files on the host root. It never
// maps onto the host root now.
func TestDirRootNeverMapsOntoTheHostRoot(t *testing.T) {
	for _, dir := range []string{"", "/", "//", "relative/dir"} {
		t.Run(dir, func(t *testing.T) {
			_, err := NewDirRoot("kube1", dir)
			require.Error(t, err)
			require.ErrorContains(t, err, `slot "kube1"`)

			require.Panics(t, func() { _ = DirRoot{SlotName: "kube1", Dir: dir}.Path("/etc/x") })
		})
	}
}

func TestNewDirRoot(t *testing.T) {
	r, err := NewDirRoot("kube2", "/var/lib/machines/kube2")
	require.NoError(t, err)
	require.Equal(t, DirRoot{SlotName: "kube2", Dir: "/var/lib/machines/kube2"}, r)
}

func TestSlotNameCarriesOnlyTheSlot(t *testing.T) {
	var ref SlotRef = SlotName("kube2")
	require.Equal(t, "kube2", ref.Slot())

	_, isRoot := ref.(NodeRoot)
	require.False(t, isRoot, "a slot name has no filesystem view")
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

func TestFakeBackendPrepareRejectsAMissingDir(t *testing.T) {
	_, err := (&FakeBackend{}).Prepare(context.Background(), NodeSpec{Slot: "kube1"})
	require.ErrorContains(t, err, "no root directory")
}

func TestFakeBackendPrepareError(t *testing.T) {
	f := &FakeBackend{PrepareErr: errors.New("nope")}
	if _, err := f.Prepare(context.Background(), NodeSpec{Slot: "kube1"}); err == nil {
		t.Fatal("expected error")
	}
}
