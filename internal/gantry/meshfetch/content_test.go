// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// contentRoot lays out a containerd content store in a temporary directory.
type contentRoot struct {
	t    *testing.T
	root string
}

func newContentRoot(t *testing.T) *contentRoot {
	t.Helper()

	root := t.TempDir()
	for _, d := range []string{"blobs/sha256", "ingest"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return &contentRoot{t: t, root: root}
}

func testDigest(i int) string { return fmt.Sprintf("sha256:%064x", i) }

func (c *contentRoot) blobPath(d string) string {
	return filepath.Join(c.root, "blobs", "sha256", hexOf(d))
}

func (c *contentRoot) commitBlob(d string, b []byte) {
	c.t.Helper()

	if err := os.WriteFile(c.blobPath(d), b, 0o444); err != nil {
		c.t.Fatal(err)
	}
}

// ingest creates an in-progress ingest of d as CRI names it, with data so
// far, and returns its directory.
func (c *contentRoot) ingest(key, d string, total int64, data []byte) string {
	c.t.Helper()

	dir := filepath.Join(c.root, "ingest", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.t.Fatal(err)
	}

	files := map[string][]byte{
		"ref":  []byte("k8s.io/1/layer-" + d),
		"data": data,
	}
	if total >= 0 {
		files["total"] = []byte(strconv.FormatInt(total, 10))
	}

	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			c.t.Fatal(err)
		}
	}

	return dir
}

func (c *contentRoot) open(t *testing.T) *DirContent {
	t.Helper()

	dc, err := NewDirContent(c.root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	dc.scanInterval = time.Nanosecond

	return dc
}

func TestDirContentCommittedBlob(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)
	d := testDigest(1)
	data := pattern(1, 10000)

	if avail, total := dc.Available(d); avail != 0 || total != -1 {
		t.Fatalf("absent blob: %d/%d", avail, total)
	}

	cr.commitBlob(d, data)

	if avail, total := dc.Available(d); avail != 10000 || total != 10000 {
		t.Fatalf("committed blob: %d/%d", avail, total)
	}

	buf := make([]byte, 100)
	if _, err := dc.ReadAt(d, buf, 5000); err != nil || !bytes.Equal(buf, data[5000:5100]) {
		t.Fatalf("ReadAt: %v", err)
	}

	// A read past the end is short.
	if _, err := dc.ReadAt(d, buf, 9950); !errors.Is(err, errContentShort) {
		t.Fatalf("short read: %v", err)
	}
}

// An ingest is found through its ref; its readable size is its data
// file, capped at total, and reads continue after the commit rename.
func TestDirContentIngestAndCommit(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)
	d := testDigest(2)
	data := pattern(2, 8192)

	dir := cr.ingest("abc", d, 8192, data[:3000])

	if avail, total := dc.Available(d); avail != 3000 || total != 8192 {
		t.Fatalf("ingest: %d/%d", avail, total)
	}

	buf := make([]byte, 1000)
	if _, err := dc.ReadAt(d, buf, 2000); err != nil || !bytes.Equal(buf, data[2000:3000]) {
		t.Fatalf("ReadAt ingest: %v", err)
	}

	// containerd appends.
	f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = f.Write(data[3000:])
	_ = f.Close()

	if avail, _ := dc.Available(d); avail != 8192 {
		t.Fatalf("after append: %d", avail)
	}

	// Commit: rename data into blobs and remove the ingest.
	if err := os.Rename(filepath.Join(dir, "data"), cr.blobPath(d)); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	// The committed blob still reads.
	if _, err := dc.ReadAt(d, buf, 7000); err != nil || !bytes.Equal(buf, data[7000:8000]) {
		t.Fatalf("ReadAt after commit: %v", err)
	}

	if avail, total := dc.Available(d); avail != 8192 || total != 8192 {
		t.Fatalf("after commit: %d/%d", avail, total)
	}
}

// An ingest without a total has an unknown size; one whose data file is
// longer than total is capped.
func TestDirContentIngestTotal(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)

	cr.ingest("a", testDigest(3), -1, make([]byte, 100))

	if avail, total := dc.Available(testDigest(3)); avail != 100 || total != -1 {
		t.Fatalf("no total: %d/%d", avail, total)
	}

	cr.ingest("b", testDigest(4), 50, make([]byte, 100))

	if avail, total := dc.Available(testDigest(4)); avail != 50 || total != 50 {
		t.Fatalf("capped: %d/%d", avail, total)
	}
}

// A deleted blob is no longer read or available.
func TestDirContentDeletedBlob(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)
	d := testDigest(5)

	cr.commitBlob(d, make([]byte, 4096))

	if _, err := dc.ReadAt(d, make([]byte, 10), 0); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(cr.blobPath(d)); err != nil {
		t.Fatal(err)
	}

	if _, err := dc.ReadAt(d, make([]byte, 10), 0); err == nil {
		t.Fatal("read of a deleted blob succeeded")
	}

	if avail, _ := dc.Available(d); avail != 0 {
		t.Fatalf("deleted blob available: %d", avail)
	}
}

// A truncated ingest (a restarted download) makes reads short.
func TestDirContentTruncatedIngest(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)
	d := testDigest(6)
	dir := cr.ingest("t", d, 8192, make([]byte, 8192))

	if _, err := dc.ReadAt(d, make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(filepath.Join(dir, "data"), 0); err != nil {
		t.Fatal(err)
	}

	if _, err := dc.ReadAt(d, make([]byte, 100), 4000); !errors.Is(err, errContentShort) {
		t.Fatalf("want short read, got %v", err)
	}
}

func TestDirContentRejectsBadDigests(t *testing.T) {
	cr := newContentRoot(t)
	dc := cr.open(t)

	for _, d := range []string{"", "sha256:../../etc/passwd", "sha512:" + hexOf(testDigest(1)), "sha256:ABC", testDigest(1) + "0"} {
		if avail, _ := dc.Available(d); avail != 0 {
			t.Errorf("%q available", d)
		}

		if _, err := dc.ReadAt(d, make([]byte, 1), 0); err == nil {
			t.Errorf("%q read", d)
		}
	}
}

func TestNewDirContentRequiresReadableRoot(t *testing.T) {
	if _, err := NewDirContent("", nil); err == nil {
		t.Fatal("empty root accepted")
	}

	if _, err := NewDirContent(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("missing root accepted")
	}
}

// A permission error disables the source instead of failing every call.
func TestDirContentPermissionErrorDisables(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}

	cr := newContentRoot(t)
	dc := cr.open(t)
	d := testDigest(7)

	cr.commitBlob(d, make([]byte, 100))

	ing := filepath.Join(cr.root, "blobs", "sha256")
	if err := os.Chmod(ing, 0); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(ing, 0o755) })

	if avail, _ := dc.Available(d); avail != 0 {
		t.Fatalf("unreadable blob available: %d", avail)
	}

	_ = os.Chmod(ing, 0o755)

	if avail, _ := dc.Available(d); avail != 0 {
		t.Fatal("source not disabled after a permission error")
	}
}

func TestRefHex(t *testing.T) {
	h := hexOf(testDigest(9))

	for ref, want := range map[string]string{
		"layer-sha256:" + h:           h,
		"k8s.io/12/layer-sha256:" + h: h,
		"gantry-sha256:" + h + "\n":   h,
		"manifest-sha256:abc":         "",
		"no digest":                   "",
	} {
		if got := refHex(ref); got != want {
			t.Errorf("refHex(%q) = %q, want %q", ref, got, want)
		}
	}
}
