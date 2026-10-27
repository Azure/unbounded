// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// unprivilegedStatus is the part of /proc/self/status that
// verifyDroppedPrivileges reads, as it looks after a successful switch.
const unprivilegedStatus = "Name:\tgantry\n" +
	"Uid:\t65532\t65532\t65532\t65532\n" +
	"Gid:\t0\t0\t0\t0\n" +
	"CapInh:\t0000000000000000\n" +
	"CapPrm:\t0000000000000000\n" +
	"CapEff:\t0000000000000000\n" +
	"CapBnd:\t00000000a80425fb\n"

// fakeAgentRuntime starts as root (euid 0) unless told otherwise and records
// startup operations in order. Each operation succeeds unless its error is set.
// Recorded chowns are applied for real as the test user, which any user may
// do, so os.Root still resolves every name the walk produces.
type fakeAgentRuntime struct {
	euid      int
	status    string
	lchownErr error
	setuidErr error
	statusErr error

	calls []string
}

func (f *fakeAgentRuntime) deps() agentRuntimeDeps {
	return agentRuntimeDeps{
		geteuid: func() int { return f.euid },
		setuid: func(uid int) error {
			f.calls = append(f.calls, fmt.Sprintf("setuid %d", uid))

			return f.setuidErr
		},
		procStatus: func() ([]byte, error) {
			f.calls = append(f.calls, "status")

			return []byte(f.status), f.statusErr
		},
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

func TestPrepareAgentRuntimeReownsIdentityDirectoryThenDropsRoot(t *testing.T) {
	dir, _ := newIdentityDirFixture(t)
	fake := &fakeAgentRuntime{status: unprivilegedStatus}

	if err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps()); err != nil {
		t.Fatalf("prepareAgentRuntimeWith: %v", err)
	}

	// Everything in the directory is re-owned before the switch. The symlink
	// itself is re-owned, and nothing under its target is visited.
	want := []string{
		"lchown . 65532:65532",
		"lchown escape 65532:65532",
		"lchown identity.key 65532:65532",
		"lchown nested 65532:65532",
		"lchown nested/child 65532:65532",
		"setuid 65532",
		"status",
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

	fake := &fakeAgentRuntime{status: unprivilegedStatus}

	if err := prepareAgentRuntimeWith(dir, discardLogger(), fake.deps()); err != nil {
		t.Fatalf("prepareAgentRuntimeWith: %v", err)
	}

	if want := []string{"lchown . 65532:65532", "setuid 65532", "status"}; !slices.Equal(fake.calls, want) {
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
			fake := &fakeAgentRuntime{status: unprivilegedStatus}

			err := prepareAgentRuntimeWith(filepath.Join(base, test.dir), discardLogger(), fake.deps())
			if err == nil || !strings.Contains(err.Error(), "without following symlinks") {
				t.Fatalf("err = %v, want the symlink refused", err)
			}

			if len(fake.calls) != 0 {
				t.Fatalf("calls = %q, want nothing re-owned and root kept", fake.calls)
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
	injected := errors.New("injected")
	rootStatus := strings.Replace(unprivilegedStatus, "Uid:\t65532\t65532\t65532\t65532", "Uid:\t0\t0\t0\t0", 1)

	for _, test := range []struct {
		name      string
		fake      fakeAgentRuntime
		wantErr   string
		wantCalls []string
	}{
		{
			name:      "re-own",
			fake:      fakeAgentRuntime{lchownErr: injected},
			wantErr:   "prepare libp2p identity directory",
			wantCalls: []string{"lchown . 65532:65532"},
		},
		{
			name:      "setuid",
			fake:      fakeAgentRuntime{setuidErr: injected},
			wantErr:   "drop root privileges: setuid 65532",
			wantCalls: []string{"lchown . 65532:65532", "setuid 65532"},
		},
		{
			name:      "status",
			fake:      fakeAgentRuntime{statusErr: injected},
			wantErr:   "drop root privileges: read process status",
			wantCalls: []string{"lchown . 65532:65532", "setuid 65532", "status"},
		},
		{
			name:      "still root",
			fake:      fakeAgentRuntime{status: rootStatus},
			wantErr:   "drop root privileges: process UIDs are 0 0 0 0",
			wantCalls: []string{"lchown . 65532:65532", "setuid 65532", "status"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := test.fake

			err := prepareAgentRuntimeWith(tempDir(t), discardLogger(), fake.deps())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, test.wantErr)
			}

			injectedFailure := fake.lchownErr != nil || fake.setuidErr != nil || fake.statusErr != nil
			if injectedFailure && !errors.Is(err, injected) {
				t.Fatalf("err = %v, want it to wrap the injected failure", err)
			}

			if !slices.Equal(fake.calls, test.wantCalls) {
				t.Fatalf("calls = %q, want %q", fake.calls, test.wantCalls)
			}
		})
	}
}

func TestVerifyDroppedPrivileges(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  string
		wantErr string
	}{
		{name: "dropped", status: unprivilegedStatus},
		{
			name:    "saved UID still root",
			status:  strings.Replace(unprivilegedStatus, "Uid:\t65532\t65532\t65532\t65532", "Uid:\t65532\t65532\t0\t65532", 1),
			wantErr: "process UIDs are 65532 65532 0 65532",
		},
		{
			name:    "permitted capabilities kept",
			status:  strings.Replace(unprivilegedStatus, "CapPrm:\t0000000000000000", "CapPrm:\t00000000000000c1", 1),
			wantErr: "CapPrm is 00000000000000c1",
		},
		{
			name:    "effective capabilities kept",
			status:  strings.Replace(unprivilegedStatus, "CapEff:\t0000000000000000", "CapEff:\t0000000000000001", 1),
			wantErr: "CapEff is 0000000000000001",
		},
		{
			name:    "no Uid line",
			status:  "CapPrm:\t0000000000000000\nCapEff:\t0000000000000000\n",
			wantErr: "unexpected Uid line",
		},
		{
			name:    "no CapEff line",
			status:  strings.Replace(unprivilegedStatus, "CapEff:\t0000000000000000\n", "", 1),
			wantErr: "parse CapEff",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyDroppedPrivileges([]byte(test.status), agentUID)

			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyDroppedPrivileges: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, test.wantErr)
			}
		})
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

// TestPrepareAgentRuntimeDropsRootAsRoot runs the real routine in a child
// process, because the switch cannot be undone and would leave the remaining
// tests running unprivileged.
func TestPrepareAgentRuntimeDropsRootAsRoot(t *testing.T) {
	const child = "GANTRY_DROP_ROOT_TEST_CHILD"

	if dir := os.Getenv(child); dir != "" {
		dropRootInChild(t, dir)

		return
	}

	if os.Geteuid() != 0 {
		t.Skip("dropping root privileges needs root")
	}

	// The parent stays root, so it can remove the directory afterward. Like
	// /var/lib/gantry in the pod, the directory holding the identity directory
	// must let the agent user through, which t.TempDir's do not.
	base, err := os.MkdirTemp("", "gantry-init-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(base) })

	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}

	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "libp2p")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")

	cmd.Env = append(os.Environ(), child+"="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}

	for _, path := range []string{dir, filepath.Join(dir, "identity.key")} {
		if uid, _ := ownerOf(t, path); uid != agentUID {
			t.Fatalf("%s is owned by uid %d, want %d", path, uid, agentUID)
		}
	}
}

// dropRootInChild runs gantry init's routine for real on dir, then checks that
// the process is the agent user, kept its groups, cannot regain root, and can
// write the identity key the way the agent will.
func dropRootInChild(t *testing.T, dir string) {
	t.Helper()

	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}

	if err := prepareAgentRuntime(dir, discardLogger()); err != nil {
		t.Fatalf("prepareAgentRuntime: %v", err)
	}

	if uid, euid := os.Getuid(), os.Geteuid(); uid != agentUID || euid != agentUID {
		t.Fatalf("uid=%d euid=%d, want %d", uid, euid, agentUID)
	}

	kept, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(kept, groups) {
		t.Fatalf("groups = %v, want them kept as %v", kept, groups)
	}

	if err := syscall.Setuid(0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("Setuid(0) after the switch = %v, want EPERM", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "identity.key"), nil, 0o600); err != nil {
		t.Fatalf("write the identity key as the agent user: %v", err)
	}
}
