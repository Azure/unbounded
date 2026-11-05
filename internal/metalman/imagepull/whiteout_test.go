// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"testing"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

// TestWhiteoutHidesNestedLowerFiles pins that a whiteout or opaque marker
// on a directory hides every lower file below it, at any depth, while
// entries recreated in the same layer stay visible.
func TestWhiteoutHidesNestedLowerFiles(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{
			"a/b/c":       []byte("lower c"),
			"a/b/d/e/f":   []byte("lower f"),
			"o/x/y/z":     []byte("lower z"),
			"r/old":       []byte("lower old"),
			"s/kept":      []byte("lower kept"),
			"target/file": []byte("target file"),
		}},
		{Entries: []ipt.Entry{
			ipt.Whiteout("a/b"),
			ipt.Opaque("o"),
			ipt.File("o/x/new", []byte("upper new")),
			// Whiteout then recreate in the same layer: the whiteout only
			// applies to lower layers.
			ipt.Whiteout("r"),
			ipt.File("r/new", []byte("upper r new")),
			ipt.Whiteout("s/kept"),
			ipt.File("s/kept", []byte("upper kept")),
			ipt.Whiteout("target"),
			ipt.Symlink("via", "target"),
			ipt.Symlink("viaabs", "/a/../target/file"),
		}},
		// A later layer recreating the parent does not unhide lower files.
		{Entries: []ipt.Entry{ipt.Dir("a/b"), ipt.File("a/b/g", []byte("top g"))}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectNotFound(t, img, "a/b/c")
	expectNotFound(t, img, "a/b/d/e/f")
	expectFile(t, img, "a/b/g", "top g")
	expectNotFound(t, img, "o/x/y/z")
	expectFile(t, img, "o/x/new", "upper new")
	expectNotFound(t, img, "r/old")
	expectFile(t, img, "r/new", "upper r new")
	expectFile(t, img, "s/kept", "upper kept")
	// Symlinks cannot reach through a whited-out directory.
	expectNotFound(t, img, "via/file")
	expectNotFound(t, img, "viaabs")
	expectNotFound(t, img, "target/file")
}

// TestUpperParentSymlinkRedirectsLowerDirectory pins that an upper symlink
// replacing a lower directory resolves through its target, including when
// the target walks above the root with "..".
func TestUpperParentSymlinkRedirectsLowerDirectory(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{
			"a/b": []byte("lower a/b"),
			"x/b": []byte("x/b"),
		}},
		{Entries: []ipt.Entry{ipt.Symlink("a", "../../../x")}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "a/b", "x/b")
}

// TestWhiteoutMarkersNotServed pins that whiteout and opaque marker entries
// are never served as files, and that tar names with "./" prefixes or
// trailing slashes are normalized for both content and whiteouts.
func TestWhiteoutMarkersNotServed(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Entries: []ipt.Entry{
			ipt.File("./d/lower", []byte("lower")),
			ipt.File("./d/gone", []byte("gone")),
		}},
		{Entries: []ipt.Entry{
			ipt.Dir("./d/"),
			ipt.Whiteout("./d/gone"),
			ipt.Opaque("e"),
			ipt.File("./d/upper", []byte("upper")),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	expectFile(t, img, "d/lower", "lower")
	expectFile(t, img, "d/upper", "upper")
	expectNotFound(t, img, "d/gone")
	expectNotFound(t, img, "d/.wh.gone")
	expectNotFound(t, img, "e/.wh..wh..opq")
}
