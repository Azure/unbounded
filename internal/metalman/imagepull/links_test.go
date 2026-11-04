// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"errors"
	"testing"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

func expectFile(t *testing.T, img *imagepull.Image, name, want string) {
	t.Helper()

	data, got, err := img.ReadFile(t.Context(), name)
	if err != nil {
		t.Errorf("ReadFile(%s): %v", name, err)
		return
	}

	if string(data) != want || got != name {
		t.Errorf("ReadFile(%s) = %q (%s), want %q", name, data, got, want)
	}
}

func expectNotFound(t *testing.T, img *imagepull.Image, name string) {
	t.Helper()

	if _, _, err := img.ReadFile(t.Context(), name); !errors.Is(err, imagepull.ErrNotFound) {
		t.Errorf("ReadFile(%s): err = %v, want ErrNotFound", name, err)
	}
}

func TestUpperNonDirectoryHidesLowerDirectory(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{
			"disk/sub/a":   []byte("lower a"),
			"disk/sub/b":   []byte("lower b"),
			"disk/link/a":  []byte("lower link a"),
			"other/a":      []byte("other a"),
			"disk/keep/ok": []byte("ok"),
		}},
		{Entries: []ipt.Entry{
			ipt.File("disk/sub", []byte("now a file")),
			ipt.Symlink("disk/link", "../other"),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "disk/sub", "now a file")
	expectNotFound(t, img, "disk/sub/a")
	expectNotFound(t, img, "disk/sub/b")
	// The upper symlink replaces the lower directory.
	expectFile(t, img, "disk/link/a", "other a")
	expectFile(t, img, "disk/keep/ok", "ok")
}

func TestUpperDirectoryHidesLowerSymlink(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Entries: []ipt.Entry{ipt.Symlink("disk", "boot"), ipt.File("boot/a", []byte("boot a"))}},
		{Files: map[string][]byte{"disk/b": []byte("disk b")}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "disk/b", "disk b")
	expectNotFound(t, img, "disk/a")
}

func TestParentDirectorySymlinks(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Entries: []ipt.Entry{
			ipt.File("boot/vmlinuz", []byte("kernel")),
			ipt.File("boot/efi/grub.cfg", []byte("grub")),
			ipt.Symlink("disk", "boot"),
			ipt.Symlink("abs", "/boot"),
			ipt.Symlink("boot/up", ".."),
			ipt.Symlink("esc", "../../../../boot"),
			ipt.Symlink("chain", "disk/efi"),
			ipt.Symlink("self", "self/x"),
			ipt.File("plain", []byte("plain")),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "disk/vmlinuz", "kernel")
	expectFile(t, img, "abs/vmlinuz", "kernel")
	expectFile(t, img, "disk/up/disk/vmlinuz", "kernel")
	// Targets cannot escape the image root.
	expectFile(t, img, "esc/vmlinuz", "kernel")
	expectFile(t, img, "chain/grub.cfg", "grub")
	expectNotFound(t, img, "disk/missing")
	// A regular file used as a directory.
	expectNotFound(t, img, "plain/x")

	if _, _, err := img.ReadFile(t.Context(), "self/y"); err == nil || errors.Is(err, imagepull.ErrNotFound) {
		t.Errorf("self loop: err = %v, want link depth error", err)
	}
}

func TestHardlinksResolveWithinLayer(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{"disk/f": []byte("lower"), "disk/g": []byte("lower g")}},
		{Entries: []ipt.Entry{
			ipt.File("disk/f", []byte("upper")),
			ipt.Hardlink("disk/h", "disk/f"),
			ipt.Hardlink("disk/hh", "disk/h"),
			// The target is not in this layer: resolved through the image.
			ipt.Hardlink("disk/hg", "disk/g"),
		}},
		// A later layer replaces the hard link target; the link still
		// refers to its own layer's content.
		{Files: map[string][]byte{"disk/f": []byte("top")}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "disk/h", "upper")
	expectFile(t, img, "disk/hh", "upper")
	expectFile(t, img, "disk/hg", "lower g")
	expectFile(t, img, "disk/f", "top")
}

func TestOpenPrecedenceByArgumentOrder(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{"disk/static.ipxe": []byte("lower static")}},
		{Entries: []ipt.Entry{
			ipt.File("disk/static.ipxe", []byte("static")),
			ipt.File("disk/boot.ipxe.tmpl", []byte("template")),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"disk/boot.ipxe.tmpl", "disk/static.ipxe"}, "template"},
		{[]string{"disk/static.ipxe", "disk/boot.ipxe.tmpl"}, "static"},
	} {
		data, _, err := img.ReadFile(t.Context(), tc.names...)
		if err != nil || string(data) != tc.want {
			t.Errorf("ReadFile(%v) = %q, %v, want %q", tc.names, data, err, tc.want)
		}
	}
}
