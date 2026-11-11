// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type cpioEntry struct {
	ino, mode, uid, gid, nlink, mtime uint32
	name                              string
	data                              []byte
}

// readCPIO parses a newc archive strictly, including padding and trailer.
func readCPIO(t *testing.T, b []byte) []cpioEntry {
	t.Helper()

	var out []cpioEntry

	off := 0

	for {
		if len(b)-off < 110 {
			t.Fatalf("truncated header at %d", off)
		}

		h := b[off : off+110]
		if string(h[:6]) != cpioMagic {
			t.Fatalf("bad magic %q at %d", h[:6], off)
		}

		field := func(i int) uint32 {
			v, err := strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 32)
			if err != nil {
				t.Fatalf("bad field %d: %v", i, err)
			}

			return uint32(v)
		}

		e := cpioEntry{ino: field(0), mode: field(1), uid: field(2), gid: field(3), nlink: field(4), mtime: field(5)}
		size, nameSize := int(field(6)), int(field(11))

		for i := 7; i <= 10; i++ {
			if field(i) != 0 {
				t.Fatalf("dev field %d nonzero", i)
			}
		}

		if field(12) != 0 {
			t.Fatalf("check nonzero")
		}

		off += 110
		e.name = string(b[off : off+nameSize-1])

		if b[off+nameSize-1] != 0 {
			t.Fatalf("name %q not NUL terminated", e.name)
		}

		off += nameSize
		for ; off%4 != 0; off++ {
			if b[off] != 0 {
				t.Fatalf("nonzero name padding")
			}
		}

		e.data = b[off : off+size]

		off += size
		for ; off%4 != 0; off++ {
			if b[off] != 0 {
				t.Fatalf("nonzero data padding")
			}
		}

		if e.name == cpioTrailer {
			if off != len(b) {
				t.Fatalf("trailing bytes after trailer")
			}

			return out
		}

		out = append(out, e)
	}
}

func TestWriteCPIO(t *testing.T) {
	files := []CPIOFile{
		{Path: "a/b/c.txt", Mode: 0o644, Data: []byte("hello")},
		{Path: "a/b/d", Mode: 0o600, Data: []byte("1234")},
		{Path: "a/e", Mode: fs.ModeDir | 0o700},
		{Path: "a/e/f", Mode: 0o644},
		{Path: "top", Mode: 0o644, Data: []byte("xyz")},
	}

	var buf bytes.Buffer
	if err := WriteCPIO(&buf, files); err != nil {
		t.Fatal(err)
	}

	if buf.Len()%4 != 0 {
		t.Fatalf("archive length %d not 4-aligned", buf.Len())
	}

	got := readCPIO(t, buf.Bytes())
	want := []struct {
		name  string
		mode  uint32
		nlink uint32
		data  string
	}{
		{"a", 0o40755, 2, ""},
		{"a/b", 0o40755, 2, ""},
		{"a/b/c.txt", 0o100644, 1, "hello"},
		{"a/b/d", 0o100600, 1, "1234"},
		{"a/e", 0o40700, 2, ""},
		{"a/e/f", 0o100644, 1, ""},
		{"top", 0o100644, 1, "xyz"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}

	for i, w := range want {
		g := got[i]
		if g.name != w.name || g.mode != w.mode || g.nlink != w.nlink || string(g.data) != w.data || g.ino != uint32(i+1) || g.uid != 0 || g.gid != 0 || g.mtime != 0 {
			t.Errorf("entry %d = %+v, want %+v", i, g, w)
		}
	}

	var again bytes.Buffer
	if err := WriteCPIO(&again, files); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Fatal("output is not deterministic")
	}
}

func TestWriteCPIOEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCPIO(&buf, nil); err != nil {
		t.Fatal(err)
	}

	if got := readCPIO(t, buf.Bytes()); len(got) != 0 {
		t.Fatalf("unexpected entries %+v", got)
	}
}

func TestWriteCPIOPadding(t *testing.T) {
	// Exercise every name and data length residue modulo 4.
	for n := 1; n <= 8; n++ {
		for d := 0; d <= 4; d++ {
			var buf bytes.Buffer

			name := strings.Repeat("n", n)
			if err := WriteCPIO(&buf, []CPIOFile{{Path: name, Mode: 0o644, Data: bytes.Repeat([]byte{'x'}, d)}}); err != nil {
				t.Fatal(err)
			}

			got := readCPIO(t, buf.Bytes())
			if len(got) != 1 || got[0].name != name || len(got[0].data) != d {
				t.Fatalf("n=%d d=%d: %+v", n, d, got)
			}
		}
	}
}

func TestWriteCPIORejects(t *testing.T) {
	cases := map[string][]CPIOFile{
		"empty":          {{Path: "", Mode: 0o644}},
		"absolute":       {{Path: "/etc/x", Mode: 0o644}},
		"dotdot":         {{Path: "a/../b", Mode: 0o644}},
		"leading dotdot": {{Path: "../b", Mode: 0o644}},
		"dot":            {{Path: "./b", Mode: 0o644}},
		"double slash":   {{Path: "a//b", Mode: 0o644}},
		"trailing slash": {{Path: "a/", Mode: 0o644}},
		"nul":            {{Path: "a\x00b", Mode: 0o644}},
		"trailer":        {{Path: cpioTrailer, Mode: 0o644}},
		"duplicate":      {{Path: "a", Mode: 0o644}, {Path: "a", Mode: 0o644}},
		"dir after implicit": {
			{Path: "a/b", Mode: 0o644},
			{Path: "a", Mode: fs.ModeDir | 0o755},
		},
		"file then child": {{Path: "a", Mode: 0o644}, {Path: "a/b", Mode: 0o644}},
		"symlink":         {{Path: "a", Mode: fs.ModeSymlink | 0o777}},
		"setuid":          {{Path: "a", Mode: fs.ModeSetuid | 0o755}},
		"dir with data":   {{Path: "a", Mode: fs.ModeDir | 0o755, Data: []byte("x")}},
	}

	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if err := WriteCPIO(&bytes.Buffer{}, files); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteCPIOWriterError(t *testing.T) {
	if err := WriteCPIO(failWriter{}, []CPIOFile{{Path: "a", Mode: 0o644}}); err == nil {
		t.Fatal("expected writer error")
	}
}

func TestWriteCPIOWithSystemCPIO(t *testing.T) {
	bin, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("cpio not installed")
	}

	var buf bytes.Buffer
	if err := WriteCPIO(&buf, []CPIOFile{{Path: "x/y/z", Mode: 0o644, Data: []byte("data")}}); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "a.cpio")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cmd := exec.Command(bin, "-t", "--quiet")
	cmd.Stdin = f

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cpio -t: %v: %s", err, out)
	}

	if got := strings.TrimSpace(string(out)); got != "x\nx/y\nx/y/z" {
		t.Fatalf("cpio -t = %q", got)
	}
}
