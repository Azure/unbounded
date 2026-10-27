// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeAgentRuntime starts as root (euid 0) unless told otherwise and records
// each chown in order. A chown fails if lchownErr is set; otherwise it is
// applied for real as the test user, which any user may do, so os.Root still
// resolves every name the walk produces.
type fakeAgentRuntime struct {
	euid      int
	lchownErr error

	calls []string
}

func (f *fakeAgentRuntime) deps() agentRuntimeDeps {
	return agentRuntimeDeps{
		geteuid: func() int { return f.euid },
		lchown: func(root *os.Root, name string, uid, gid int) error {
			f.calls = append(f.calls, fmt.Sprintf("lchown %s %d:%d", name, uid, gid))

			if f.lchownErr != nil {
				return f.lchownErr
			}

			return root.Lchown(name, os.Getuid(), os.Getgid())
		},
	}
}

// tempDir is t.TempDir with symlinks resolved, because the identity directory
// step refuses a path that passes through one.
func tempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// newIdentityDirFixture returns an identity directory holding the key, a nested
// directory, and a symlink to a sibling directory outside it.
func newIdentityDirFixture(t *testing.T) (dir, outside string) {
	t.Helper()

	base := tempDir(t)
	dir = filepath.Join(base, "libp2p")
	outside = filepath.Join(base, "outside")

	for _, path := range []string{filepath.Join(dir, "nested"), outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{
		filepath.Join(dir, "identity.key"),
		filepath.Join(dir, "nested", "child"),
		filepath.Join(outside, "secret"),
	} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	return dir, outside
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Mode().Perm()
}

func ownerOf(t *testing.T, path string) (uid, gid int) {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no ownership information for %s", path)
	}

	return int(stat.Uid), int(stat.Gid)
}

func TestPrepareAgentRuntimeLeavesUnprivilegedProcessAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "libp2p")
	fake := &fakeAgentRuntime{euid: agentUID}

	if err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps()); err != nil {
		t.Fatalf("prepareAgentRuntimeWith: %v", err)
	}

	if len(fake.calls) != 0 {
		t.Fatalf("unprivileged start ran %q, want nothing", fake.calls)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unprivileged start touched %s: %v", dir, err)
	}
}

func TestPrepareAgentRuntimeReownsIdentityDirectory(t *testing.T) {
	dir, _ := newIdentityDirFixture(t)
	fake := &fakeAgentRuntime{}

	if err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps()); err != nil {
		t.Fatalf("prepareAgentRuntimeWith: %v", err)
	}

	// Everything in the directory is re-owned. The symlink itself is
	// re-owned, and nothing under its target is visited.
	want := []string{
		"lchown . 65532:65532",
		"lchown escape 65532:65532",
		"lchown identity.key 65532:65532",
		"lchown nested 65532:65532",
		"lchown nested/child 65532:65532",
	}
	if !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %q, want %q", fake.calls, want)
	}

	if mode := modeOf(t, dir); mode != identityDirMode {
		t.Fatalf("identity directory mode = %v, want %v", mode, identityDirMode)
	}
}

func TestPrepareAgentRuntimeCreatesMissingIdentityDirectory(t *testing.T) {
	base := tempDir(t)
	dir := filepath.Join(base, "state", "libp2p")

	// Created parents must stay traversable by the agent. Compare them with a
	// directory made the same way, so the expectation follows the umask.
	reference := filepath.Join(base, "reference")
	if err := os.Mkdir(reference, 0o755); err != nil {
		t.Fatal(err)
	}

	fake := &fakeAgentRuntime{}

	if err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps()); err != nil {
		t.Fatalf("prepareAgentRuntimeWith: %v", err)
	}

	if want := []string{"lchown . 65532:65532"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %q, want %q", fake.calls, want)
	}

	if mode := modeOf(t, dir); mode != identityDirMode {
		t.Fatalf("identity directory mode = %v, want %v", mode, identityDirMode)
	}

	if mode, want := modeOf(t, filepath.Dir(dir)), modeOf(t, reference); mode != want {
		t.Fatalf("created parent mode = %v, want %v", mode, want)
	}
}

// TestPrepareAgentRuntimeRefusesSymlinksInIdentityPath plants a link where the
// identity directory or one of its parents belongs, as an agent that owns the
// parent could. Following it would re-own and restrict whatever it points at,
// so startup must stop before changing anything.
func TestPrepareAgentRuntimeRefusesSymlinksInIdentityPath(t *testing.T) {
	for _, test := range []struct {
		name string
		link string
		dir  string
	}{
		{name: "identity directory", link: "libp2p", dir: "libp2p"},
		{name: "parent directory", link: "state", dir: "state/libp2p"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := tempDir(t)

			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(target, filepath.Join(base, test.link)); err != nil {
				t.Fatal(err)
			}

			mode := modeOf(t, target)
			fake := &fakeAgentRuntime{}

			err := prepareAgentRuntimeWith(filepath.Join(base, test.dir), discardLogger(), fake.deps())
			if err == nil || !strings.Contains(err.Error(), "without following symlinks") {
				t.Fatalf("err = %v, want the symlink refused", err)
			}

			if len(fake.calls) != 0 {
				t.Fatalf("calls = %q, want nothing re-owned", fake.calls)
			}

			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}

			if len(entries) != 0 || modeOf(t, target) != mode {
				t.Fatalf("symlink target changed: %d entries, mode %v; want none and %v", len(entries), modeOf(t, target), mode)
			}
		})
	}
}

func TestPrepareAgentRuntimeStopsAtFirstFailure(t *testing.T) {
	dir, _ := newIdentityDirFixture(t)
	mode := modeOf(t, dir)
	injected := errors.New("injected")
	fake := &fakeAgentRuntime{lchownErr: injected}

	err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps())
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "prepare libp2p identity directory") {
		t.Fatalf("err = %v, want the injected failure wrapped", err)
	}

	// The walk stops at its first entry, and the directory is never restricted.
	if want := []string{"lchown . 65532:65532"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %q, want %q", fake.calls, want)
	}

	if got := modeOf(t, dir); got != mode {
		t.Fatalf("identity directory mode = %v, want it left at %v", got, mode)
	}
}

func TestPrepareIdentityDirChangesOwnershipAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving files to another user needs root")
	}

	dir, outside := newIdentityDirFixture(t)

	// The second pass is a container restart: the directory is already
	// restricted to the agent, so root has to get through it again.
	for range 2 {
		if err := prepareIdentityDir(dir, (*os.Root).Lchown); err != nil {
			t.Fatalf("prepareIdentityDir: %v", err)
		}
	}

	for _, name := range []string{".", "escape", "identity.key", "nested", "nested/child"} {
		if uid, gid := ownerOf(t, filepath.Join(dir, name)); uid != agentUID || gid != identityDirGID {
			t.Fatalf("%s is owned by %d:%d, want %d:%d", name, uid, gid, agentUID, identityDirGID)
		}
	}

	if uid, gid := ownerOf(t, filepath.Join(outside, "secret")); uid != 0 || gid != 0 {
		t.Fatalf("symlink target was re-owned to %d:%d", uid, gid)
	}

	if mode := modeOf(t, dir); mode != identityDirMode {
		t.Fatalf("identity directory mode = %v, want %v", mode, identityDirMode)
	}
}
