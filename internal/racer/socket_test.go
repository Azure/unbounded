// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrepareSocketDirectory(t *testing.T) {
	root := t.TempDir()

	const mode = os.ModeSetgid | 0o770
	if err := os.Chmod(root, mode); err != nil {
		t.Fatal(err)
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"client", "origin"} {
		socket := filepath.Join(root, "cache", kind, "socket")
		for range 2 {
			if err := PrepareSocketDirectory(socket); err != nil {
				t.Fatal(err)
			}
		}

		for _, path := range []string{filepath.Join(root, "cache"), filepath.Dir(socket)} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			if info.Mode()&(os.ModePerm|os.ModeSetgid) != mode || info.Sys().(*syscall.Stat_t).Gid != rootInfo.Sys().(*syscall.Stat_t).Gid {
				t.Fatalf("directory %s did not inherit shared-group permissions: %v", path, info)
			}
		}
	}

	if err := os.Symlink(filepath.Join(root, "cache"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"relative/socket", filepath.Join(root, "alias/client/socket"), filepath.Join(root, "file/client/socket")} {
		if err := PrepareSocketDirectory(path); err == nil {
			t.Fatalf("accepted invalid parent: %q", path)
		}
	}
}
