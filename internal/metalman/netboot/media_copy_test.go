// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteMediaTreeStreamsFiles(t *testing.T) {
	src := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB

	from := filepath.Join(src, "initrd")
	if err := os.WriteFile(from, big, 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	files := []mediaFile{
		{Path: "casper/initrd", From: from},
		{Path: "boot/grub/grub.cfg", Data: []byte("cfg")},
	}

	if err := writeMediaTree(root, files); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(root, "casper", "initrd"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("copied payload mismatch: err=%v len=%d", err, len(got))
	}

	got, err = os.ReadFile(filepath.Join(root, "boot", "grub", "grub.cfg"))
	if err != nil || string(got) != "cfg" {
		t.Fatalf("inline payload mismatch: err=%v %q", err, got)
	}

	// Overwriting a longer existing file must truncate.
	if err := os.WriteFile(from, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeMediaTree(root, files[:1]); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(filepath.Join(root, "casper", "initrd")); string(got) != "short" {
		t.Fatalf("expected truncated copy, got %d bytes", len(got))
	}
}

func TestWriteMediaTreeMissingSource(t *testing.T) {
	err := writeMediaTree(t.TempDir(), []mediaFile{{Path: "x", From: filepath.Join(t.TempDir(), "missing")}})
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func TestCopyMediaFileDestinationError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyMediaFile(filepath.Join(t.TempDir(), "nodir", "d"), src); err == nil {
		t.Fatal("expected error for missing destination directory")
	}
}
