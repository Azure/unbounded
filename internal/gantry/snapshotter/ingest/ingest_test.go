// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// memVolume is a catalog device backed by memory. The catalog's own tests
// cover OCC conflict handling; here the volume only has to be a correct byte
// array so the ingester's use of the store can be exercised.
type memVolume struct {
	mu   sync.Mutex
	data []byte
}

func newMemVolume(size int) *memVolume { return &memVolume{data: make([]byte, size)} }

func (v *memVolume) ReadAt(p []byte, off int64) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if off < 0 || off+int64(len(p)) > int64(len(v.data)) {
		return 0, io.EOF
	}

	return copy(p, v.data[off:]), nil
}

func (v *memVolume) WriteAt(p []byte, off int64) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if off < 0 || off+int64(len(p)) > int64(len(v.data)) {
		return 0, io.ErrShortWrite
	}

	return copy(v.data[off:], p), nil
}

// fileLocator points every address at one file standing in for a segment
// device.
type fileLocator struct {
	path string
	err  error
}

func (l fileLocator) Locate(addr segment.Address) (string, uint64, uint64, error) {
	if l.err != nil {
		return "", 0, 0, l.err
	}

	return l.path, addr.ByteOffset(), addr.Span(), nil
}

// segmentLocator gives each segment its own file, which is what a node with
// more than one segment of the image volume mapped actually has.
type segmentLocator struct {
	paths map[uint32]string
}

func (l segmentLocator) Locate(addr segment.Address) (string, uint64, uint64, error) {
	path, ok := l.paths[addr.Segment]
	if !ok {
		return "", 0, 0, fmt.Errorf("segment %d is not mapped", addr.Segment)
	}

	return path, addr.ByteOffset(), addr.Span(), nil
}

// bytesOpener hands back a fixed tar payload.
type bytesOpener struct {
	data []byte
	err  error

	// failFirst makes the first Open fail and every later one succeed,
	// which is what a layer blob that has not finished downloading yet
	// looks like.
	failFirst error

	opens int
}

func (o *bytesOpener) Open(context.Context, Request) (ReadCloser, error) {
	if o.failFirst != nil {
		err := o.failFirst
		o.failFirst = nil

		return nil, err
	}

	if o.err != nil {
		return nil, o.err
	}

	o.opens++

	return io.NopCloser(bytes.NewReader(o.data)), nil
}

// fakeBuilder returns a Builder whose run hook writes a deterministic image of
// the requested size instead of shelling out to erofs-utils.
func fakeBuilder(t *testing.T, image []byte) (*Builder, *int) {
	t.Helper()

	calls := 0
	b := NewBuilder()
	b.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++

		out := args[len(args)-2]
		if err := os.WriteFile(out, image, 0o600); err != nil {
			return nil, err
		}

		return nil, nil
	}

	return b, &calls
}

func digestOf(b byte) catalog.Digest {
	var d catalog.Digest

	for i := range d {
		d[i] = b
	}

	return d
}

func sum(data []byte) catalog.Digest {
	var d catalog.Digest

	copy(d[:], func() []byte { s := sha256.Sum256(data); return s[:] }())

	return d
}

// newStore formats a catalog with one open segment of the given page count.
func newStore(t *testing.T, pages uint32) *catalog.Store {
	t.Helper()

	_, s := newStoreOn(t, pages)

	return s
}

// newStoreOn is newStore, handing back the volume as well so a test can open a
// second reader on the same catalog and see what another node would see.
func newStoreOn(t *testing.T, pages uint32) (*memVolume, *catalog.Store) {
	t.Helper()

	vol := newMemVolume(64 * catalog.BlockBytes)

	if err := catalog.Format(vol, catalog.FormatOptions{Bytes: 64 * catalog.BlockBytes}); err != nil {
		t.Fatalf("format: %v", err)
	}

	s, err := catalog.Open(vol)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := s.AddSegment(1, pages, 0); err != nil {
		t.Fatalf("add segment: %v", err)
	}

	if err := s.SetOpenSegment(1); err != nil {
		t.Fatalf("open segment: %v", err)
	}

	return vol, s
}

// deviceFile creates a sparse file big enough to hold pages 4 MiB pages.
func deviceFile(t *testing.T, pages int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "segment.img")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	if err := f.Truncate(int64(pages) * segment.PageBytes); err != nil {
		t.Fatalf("truncate device: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close device: %v", err)
	}

	return path
}

func openPlain(path string) (Device, error) { return os.OpenFile(path, os.O_RDWR, 0) }

func newIngester(t *testing.T, opts Options) *Ingester {
	t.Helper()

	if opts.WorkDir == "" {
		opts.WorkDir = t.TempDir()
	}

	if opts.Open == nil {
		opts.Open = openPlain
	}

	if opts.Free == nil {
		// These tests are about ingest, not about the disk underneath
		// them. A fixed generous number keeps them from depending on
		// how much room the machine running them happens to have.
		opts.Free = func(string) (uint64, error) { return 1 << 40, nil }
	}

	i, err := New(opts)
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	return i
}

func TestUUIDFor(t *testing.T) {
	a := UUIDFor("sha256:abc")
	b := UUIDFor("sha256:abc")

	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}

	if UUIDFor("sha256:def") == a {
		t.Fatal("distinct names produced the same uuid")
	}

	if len(a) != 36 {
		t.Fatalf("uuid %q has length %d, want 36", a, len(a))
	}

	if a[14] != '5' {
		t.Fatalf("uuid %q is not version 5", a)
	}

	if !strings.ContainsRune("89ab", rune(a[19])) {
		t.Fatalf("uuid %q has the wrong variant", a)
	}
}

func TestBuilderArgs(t *testing.T) {
	b := NewBuilder()
	b.ExtraArgs = []string{"--quiet"}

	args := b.Args(BuildOptions{TarPath: "/in.tar", OutPath: "/out.erofs", UUID: "u"})
	want := []string{"--tar=f", "--aufs", "-b4096", "-Uu", "--quiet", "/out.erofs", "/in.tar"}

	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestBuilderArgsWithoutUUID(t *testing.T) {
	args := NewBuilder().Args(BuildOptions{TarPath: "/in.tar", OutPath: "/out.erofs"})

	for _, a := range args {
		if strings.HasPrefix(a, "-U") {
			t.Fatalf("unexpected uuid argument in %v", args)
		}
	}
}

func TestBuilderBuild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.erofs")

	b, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 1234))

	size, err := b.Build(t.Context(), BuildOptions{TarPath: filepath.Join(dir, "in.tar"), OutPath: out})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if size != 1234 {
		t.Fatalf("size = %d, want 1234", size)
	}

	if *calls != 1 {
		t.Fatalf("calls = %d, want 1", *calls)
	}
}

func TestBuilderBuildValidates(t *testing.T) {
	b := NewBuilder()

	if _, err := b.Build(t.Context(), BuildOptions{OutPath: "/out"}); err == nil {
		t.Fatal("want an error for a missing tar path")
	}

	if _, err := b.Build(t.Context(), BuildOptions{TarPath: "/in"}); err == nil {
		t.Fatal("want an error for a missing output path")
	}
}

func TestBuilderBuildRemovesPartialOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.erofs")

	b := NewBuilder()
	b.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if err := os.WriteFile(args[len(args)-2], []byte("half"), 0o600); err != nil {
			return nil, err
		}

		return []byte("mkfs.erofs: out of space"), errors.New("exit 1")
	}

	_, err := b.Build(t.Context(), BuildOptions{TarPath: filepath.Join(dir, "in.tar"), OutPath: out})
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("err = %v, want ErrBuildFailed", err)
	}

	if !strings.Contains(err.Error(), "out of space") {
		t.Fatalf("err = %v, want the builder output included", err)
	}

	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Fatalf("partial output survived: %v", serr)
	}
}

func TestBuilderBuildRejectsEmptyImage(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.erofs")

	b, _ := fakeBuilder(t, nil)

	if _, err := b.Build(t.Context(), BuildOptions{TarPath: filepath.Join(dir, "in.tar"), OutPath: out}); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("err = %v, want ErrBuildFailed", err)
	}

	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Fatal("empty output survived")
	}
}

func TestTrimKeepsTheTail(t *testing.T) {
	out := bytes.Repeat([]byte("x"), 600)
	out = append(out, []byte("the real error")...)

	got := trim(out)
	if len(got) != 512 {
		t.Fatalf("len = %d, want 512", len(got))
	}

	if !strings.HasSuffix(got, "the real error") {
		t.Fatalf("tail lost: %q", got[len(got)-30:])
	}
}

func TestSpill(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("t"), 4097)

	path, size, err := Spill(dir, "l-*.tar", bytes.NewReader(payload), 0)
	if err != nil {
		t.Fatalf("spill: %v", err)
	}

	if size != uint64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatal("spilled content differs")
	}
}

func TestSpillRemovesTheFileOnError(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("boom")

	_, _, err := Spill(dir, "l-*.tar", io.MultiReader(bytes.NewReader([]byte("a")), errReader{want}), 0)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("read dir: %v", rerr)
	}

	if len(entries) != 0 {
		t.Fatalf("temp file survived: %v", entries)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestAlignedBuffer(t *testing.T) {
	for _, n := range []int{1, Alignment, segment.PageBytes} {
		buf := AlignedBuffer(n)
		if len(buf) != n {
			t.Fatalf("len = %d, want %d", len(buf), n)
		}

		if cap(buf) != n {
			t.Fatalf("cap = %d, want %d so an append cannot walk into the padding", cap(buf), n)
		}
	}
}

func TestWriteBlobPadsToAWholePage(t *testing.T) {
	path := deviceFile(t, 2)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = f.Close() }()

	payload := bytes.Repeat([]byte("z"), segment.PageBytes+7)

	got, err := WriteBlob(f, 0, bytes.NewReader(payload), uint64(len(payload)))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if got != sum(payload) {
		t.Fatal("checksum is not over the unpadded bytes")
	}

	tail := make([]byte, 16)
	if _, err := f.ReadAt(tail, segment.PageBytes+7); err != nil {
		t.Fatalf("read tail: %v", err)
	}

	if !bytes.Equal(tail, make([]byte, 16)) {
		t.Fatalf("tail padding = %q, want zeros", tail)
	}

	if err := VerifyBlob(f, 0, uint64(len(payload)), got); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestWriteBlobRejects(t *testing.T) {
	path := deviceFile(t, 1)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := WriteBlob(f, 1024, bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("want an error for an unaligned offset")
	}

	if _, err := WriteBlob(f, 0, bytes.NewReader(nil), 0); err == nil {
		t.Fatal("want an error for an empty blob")
	}

	if _, err := WriteBlob(f, 0, bytes.NewReader([]byte("ab")), 4); !errors.Is(err, ErrShortLayer) {
		t.Fatalf("err = %v, want ErrShortLayer", err)
	}

	if _, err := WriteBlob(f, 0, bytes.NewReader([]byte("abcd")), 2); !errors.Is(err, ErrLongLayer) {
		t.Fatalf("err = %v, want ErrLongLayer", err)
	}
}

func TestVerifyBlobDetectsRot(t *testing.T) {
	path := deviceFile(t, 1)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = f.Close() }()

	payload := bytes.Repeat([]byte("v"), 8192)

	got, err := WriteBlob(f, 0, bytes.NewReader(payload), uint64(len(payload)))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := f.WriteAt([]byte("!"), 4096); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	if err := VerifyBlob(f, 0, uint64(len(payload)), got); !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify", err)
	}

	if err := VerifyBlob(f, 512, 1, got); err == nil {
		t.Fatal("want an error for an unaligned offset")
	}
}

func TestNewValidates(t *testing.T) {
	base := func() Options {
		return Options{
			Catalog: newStore(t, 4),
			Locator: fileLocator{path: "/dev/null"},
			Opener:  &bytesOpener{},
			WorkDir: t.TempDir(),
		}
	}

	cases := map[string]func(*Options){
		"catalog": func(o *Options) { o.Catalog = nil },
		"locator": func(o *Options) { o.Locator = nil },
		"opener":  func(o *Options) { o.Opener = nil },
		"workdir": func(o *Options) { o.WorkDir = "" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := base()
			mutate(&opts)

			if _, err := New(opts); err == nil {
				t.Fatalf("want an error with no %s", name)
			}
		})
	}
}

func TestIngestValidatesTheRequest(t *testing.T) {
	i := newIngester(t, Options{
		Catalog: newStore(t, 4),
		Locator: fileLocator{path: deviceFile(t, 4)},
		Opener:  &bytesOpener{},
	})

	if _, err := i.Ingest(t.Context(), Request{ChainID: digestOf(2)}); err == nil {
		t.Fatal("want an error without a diff id")
	}

	if _, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1)}); err == nil {
		t.Fatal("want an error without a chain id")
	}
}

func TestIngestWritesAndPublishes(t *testing.T) {
	store := newStore(t, 8)
	device := deviceFile(t, 8)
	image := bytes.Repeat([]byte("erofs"), 100000) // 500000 bytes, one page

	builder, calls := fakeBuilder(t, image)
	opener := &bytesOpener{data: []byte("tar")}

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: device},
		Opener:  opener,
		Builder: builder,
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}

	res, err := i.Ingest(t.Context(), req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if res.Outcome != OutcomeIngested {
		t.Fatalf("outcome = %s, want ingested", res.Outcome)
	}

	if *calls != 1 {
		t.Fatalf("builder calls = %d, want 1", *calls)
	}

	if res.Blob.Address.PageCount != 1 || res.Blob.Address.ByteLength != uint64(len(image)) {
		t.Fatalf("address = %+v", res.Blob.Address)
	}

	if res.Blob.Sum != sum(image) {
		t.Fatal("published checksum does not match the image")
	}

	// The bytes are really on the device where the record says they are.
	on := make([]byte, len(image))

	f, err := os.Open(device)
	if err != nil {
		t.Fatalf("open device: %v", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.ReadAt(on, int64(res.Blob.Address.ByteOffset())); err != nil {
		t.Fatalf("read device: %v", err)
	}

	if !bytes.Equal(on, image) {
		t.Fatal("device content differs from the image")
	}

	// A second node resolves the chain out of the catalog with no work.
	blob, ok := store.Resolve(req.ChainID)
	if !ok {
		t.Fatal("chain did not resolve")
	}

	if blob.Address != res.Blob.Address {
		t.Fatalf("resolved %+v, want %+v", blob.Address, res.Blob.Address)
	}

	segs, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if segs[0].LiveBytes != segment.PageBytes {
		t.Fatalf("live bytes = %d, want one page", segs[0].LiveBytes)
	}
}

// failAccount is a catalog whose records work and whose accounting does not.
type failAccount struct {
	Catalog
}

func (failAccount) Account(uint32, int64, int64) error {
	return errors.New("accounting is broken")
}

func TestIngestSurvivesBrokenAccounting(t *testing.T) {
	store := newStore(t, 8)
	image := bytes.Repeat([]byte("erofs"), 4096)

	builder, _ := fakeBuilder(t, image)

	i := newIngester(t, Options{
		Catalog: failAccount{Catalog: store},
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}

	res, err := i.Ingest(t.Context(), req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if res.Outcome != OutcomeIngested {
		t.Fatalf("outcome = %s, want ingested", res.Outcome)
	}

	// The layer is published and resolvable, which is what the ingest was
	// for. Failing here would requeue a request whose chain already
	// resolves, so the retry would report present without ever fixing the
	// number that failed.
	if _, ok := store.Resolve(req.ChainID); !ok {
		t.Fatal("chain did not resolve")
	}

	segs, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if segs[0].LiveBytes != 0 {
		t.Fatalf("live bytes = %d, want the accounting to have been lost", segs[0].LiveBytes)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	store := newStore(t, 8)
	builder, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))
	opener := &bytesOpener{data: []byte("tar")}

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  opener,
		Builder: builder,
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}

	if _, err := i.Ingest(t.Context(), req); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	res, err := i.Ingest(t.Context(), req)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if res.Outcome != OutcomePresent {
		t.Fatalf("outcome = %s, want present", res.Outcome)
	}

	if *calls != 1 {
		t.Fatalf("builder calls = %d, want 1", *calls)
	}

	if opener.opens != 1 {
		t.Fatalf("layer opened %d times, want 1", opener.opens)
	}
}

func TestIngestLinksASharedLayer(t *testing.T) {
	store := newStore(t, 8)
	builder, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	first := Request{DiffID: digestOf(1), ChainID: digestOf(2)}
	if _, err := i.Ingest(t.Context(), first); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// The same layer at a different position in a different image: same
	// diffID, different chainID.
	second := Request{DiffID: digestOf(1), ChainID: digestOf(3)}

	res, err := i.Ingest(t.Context(), second)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if res.Outcome != OutcomeLinked {
		t.Fatalf("outcome = %s, want linked", res.Outcome)
	}

	if *calls != 1 {
		t.Fatalf("builder calls = %d, want 1: a shared layer must not be rebuilt", *calls)
	}

	a, _ := store.Resolve(first.ChainID)

	b, ok := store.Resolve(second.ChainID)
	if !ok {
		t.Fatal("linked chain did not resolve")
	}

	if a.Address != b.Address {
		t.Fatalf("shared layer resolved to two addresses: %+v and %+v", a.Address, b.Address)
	}
}

func TestIngestReportsOpenerFailure(t *testing.T) {
	want := errors.New("no such blob")
	builder, _ := fakeBuilder(t, []byte("e"))

	i := newIngester(t, Options{
		Catalog: newStore(t, 8),
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{err: want},
		Builder: builder,
	})

	if _, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestIngestReportsLocatorFailure(t *testing.T) {
	want := errors.New("segment not exported")
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))
	store := newStore(t, 8)

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{err: want},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}
	if _, err := i.Ingest(t.Context(), req); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}

	// The reservation is spent but nothing was published, so the chain must
	// still miss: a half finished ingest must never be resolvable.
	if _, ok := store.Resolve(req.ChainID); ok {
		t.Fatal("a failed ingest published a record")
	}
}

// A failed ingest must not cost the cluster its catalog.
//
// The reservation publishes its record slots before the records exist, and
// every reader stops at the first empty slot, so slots that are claimed and
// never filled stop every node in the cluster there permanently: nothing
// appended afterwards is ever seen again, by anyone. This is the test that the
// failure stays local to the node that had it.
func TestIngestFailureLeavesNoHole(t *testing.T) {
	vol, store := newStoreOn(t, 8)
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	// One ingest that dies after taking its reservation.
	broken := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{err: errors.New("segment not exported")},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	if _, err := broken.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)}); err == nil {
		t.Fatal("the broken ingest succeeded")
	}

	// Then an unrelated layer, ingested normally.
	device := deviceFile(t, 8)
	working := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: device},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	req := Request{DiffID: digestOf(3), ChainID: digestOf(4)}
	if _, err := working.Ingest(t.Context(), req); err != nil {
		t.Fatalf("the second ingest failed: %v", err)
	}

	// Another node, reading the same catalog from scratch, has to get past
	// the abandoned slots to the records that came after them.
	reader, err := catalog.Open(vol)
	if err != nil {
		t.Fatalf("open a second reader: %v", err)
	}

	if _, err := reader.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, ok := reader.Resolve(req.ChainID); !ok {
		t.Fatal("a reader stopped at the abandoned slots and never saw the layer behind them")
	}

	if _, ok := reader.Resolve(digestOf(2)); ok {
		t.Fatal("the failed ingest published a record")
	}
}

// shrinkingFree reports a different number on each call, so a test can put the
// filesystem under pressure between the two checks an ingest makes.
type shrinkingFree struct {
	mu   sync.Mutex
	vals []uint64
}

func (s *shrinkingFree) free(string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := s.vals[0]
	if len(s.vals) > 1 {
		s.vals = s.vals[1:]
	}

	return v, nil
}

// noReserve fails the test if anything takes a catalog reservation.
type noReserve struct {
	Catalog

	t *testing.T
}

func (n noReserve) Reserve(pages uint32, records int) (catalog.Reservation, error) {
	n.t.Helper()
	n.t.Fatalf("a refused ingest reserved %d pages and %d records", pages, records)

	return catalog.Reservation{}, nil
}

func TestIngestRefusesALayerTheNodeHasNoRoomFor(t *testing.T) {
	builder, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	// Less free than the reserve. Nothing may be spent, not even to find
	// out how big the layer is. A layer this node cannot hold is a local
	// problem, and taking a reservation for it would make it everyone's.
	i := newIngester(t, Options{
		Catalog:  noReserve{Catalog: newStore(t, 8), t: t},
		Locator:  fileLocator{path: deviceFile(t, 8)},
		Opener:   &bytesOpener{data: []byte("tar")},
		Builder:  builder,
		Headroom: 4 << 30,
		Free:     func(string) (uint64, error) { return 1 << 30, nil },
	})

	_, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want %v", err, ErrNoSpace)
	}

	if *calls != 0 {
		t.Fatalf("a refused ingest still ran mkfs.erofs %d times", *calls)
	}
}

func TestIngestRefusesWhenTheImageWouldNotFit(t *testing.T) {
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	// Room to spare when the layer is fetched, almost none by the time it
	// has landed: another pod on the node took the disk in between.
	free := &shrinkingFree{vals: []uint64{16 << 20, (4 << 20) + 1024}}

	i := newIngester(t, Options{
		Catalog:  noReserve{Catalog: newStore(t, 8), t: t},
		Locator:  fileLocator{path: deviceFile(t, 8)},
		Opener:   &bytesOpener{data: bytes.Repeat([]byte("t"), 2048)},
		Builder:  builder,
		Headroom: 4 << 20,
		Free:     free.free,
	})

	_, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want %v", err, ErrNoSpace)
	}
}

func TestSpillRefusesAnOversizedLayer(t *testing.T) {
	dir := t.TempDir()

	_, _, err := Spill(dir, "l-*.tar", bytes.NewReader(bytes.Repeat([]byte("t"), 4096)), 1024)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want %v", err, ErrNoSpace)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("read dir: %v", rerr)
	}

	if len(entries) != 0 {
		t.Fatalf("the partial tarball survived: %v", entries)
	}
}

func TestSpillAcceptsALayerExactlyAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("t"), 1024)

	_, size, err := Spill(dir, "l-*.tar", bytes.NewReader(payload), 1024)
	if err != nil {
		t.Fatalf("spill: %v", err)
	}

	if size != uint64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
}

func TestIngestVerifiesWhatItWrote(t *testing.T) {
	store := newStore(t, 8)
	device := deviceFile(t, 8)
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	// A device that drops every other byte on the floor stands in for a
	// fabric that acknowledged a write it did not durably take.
	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: device},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
		Open: func(path string) (Device, error) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}

			return lyingDevice{f}, nil
		},
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}
	if _, err := i.Ingest(t.Context(), req); !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify", err)
	}

	if _, ok := store.Resolve(req.ChainID); ok {
		t.Fatal("an unverified blob was published")
	}
}

// lyingDevice acknowledges writes without storing them.
type lyingDevice struct{ *os.File }

func (d lyingDevice) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func TestIngestSkipVerifyPublishesAnyway(t *testing.T) {
	store := newStore(t, 8)
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog:    store,
		Locator:    fileLocator{path: deviceFile(t, 8)},
		Opener:     &bytesOpener{data: []byte("tar")},
		Builder:    builder,
		SkipVerify: true,
		Open: func(path string) (Device, error) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}

			return lyingDevice{f}, nil
		},
	})

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2)}
	if _, err := i.Ingest(t.Context(), req); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if _, ok := store.Resolve(req.ChainID); !ok {
		t.Fatal("chain did not resolve with verification off")
	}
}

func TestIngestRollsIntoTheNextSegment(t *testing.T) {
	// One page of segment and a one page image, so the second layer arrives
	// at a segment that is exactly full.
	store := newStore(t, 1)

	if err := store.AddSegment(2, 4, 0); err != nil {
		t.Fatalf("add segment: %v", err)
	}

	devices := segmentLocator{paths: map[uint32]string{
		1: deviceFile(t, 1),
		2: deviceFile(t, 4),
	}}

	image := bytes.Repeat([]byte("erofs"), 100000)
	builder, _ := fakeBuilder(t, image)

	i := newIngester(t, Options{
		Catalog: store,
		Locator: devices,
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	first, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if first.Blob.Address.Segment != 1 {
		t.Fatalf("first layer landed in segment %d, want 1", first.Blob.Address.Segment)
	}

	second, err := i.Ingest(t.Context(), Request{DiffID: digestOf(3), ChainID: digestOf(4)})
	if err != nil {
		t.Fatalf("a layer arriving at a full segment was not ingested: %v", err)
	}

	if second.Blob.Address.Segment != 2 || second.Blob.Address.PageOffset != 0 {
		t.Fatalf("the second layer did not roll: %+v", second.Blob.Address)
	}

	// The blob is on the device the roll moved it to, not on the full one.
	on := make([]byte, len(image))

	f, err := os.Open(devices.paths[2])
	if err != nil {
		t.Fatalf("open device: %v", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.ReadAt(on, int64(second.Blob.Address.ByteOffset())); err != nil {
		t.Fatalf("read device: %v", err)
	}

	if !bytes.Equal(on, image) {
		t.Fatal("device content differs from the image")
	}

	// Both layers resolve, so nothing the roll did lost the first one.
	for _, chain := range []catalog.Digest{digestOf(2), digestOf(4)} {
		if _, ok := store.Resolve(chain); !ok {
			t.Fatalf("chain %s did not resolve after the roll", chain)
		}
	}

	segs, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	// A segment is accounted for by the pages it hands out, not by the bytes
	// of the image sitting in them.
	if segs[0].State != catalog.SegmentSealed || segs[0].LiveBytes != segment.PageBytes {
		t.Fatalf("the full segment was not sealed with its layer: %+v", segs[0])
	}

	if segs[1].State != catalog.SegmentOpen || segs[1].LiveBytes != segment.PageBytes {
		t.Fatalf("the new segment was not accounted for: %+v", segs[1])
	}
}

func TestIngestOutOfSpace(t *testing.T) {
	// One page of segment, an image that needs two.
	store := newStore(t, 1)
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), segment.PageBytes+1))

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	if _, err := i.Ingest(t.Context(), Request{DiffID: digestOf(1), ChainID: digestOf(2)}); !errors.Is(err, catalog.ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
}

func TestOutcomeString(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeUnknown:  "unknown",
		OutcomePresent:  "present",
		OutcomeLinked:   "linked",
		OutcomeIngested: "ingested",
		Outcome(99):     "unknown(99)",
	}

	for o, want := range cases {
		if got := o.String(); got != want {
			t.Fatalf("Outcome(%d) = %q, want %q", int(o), got, want)
		}
	}
}

// foreignAbandon stands in for a catalog the node re-attached away from while
// an ingest was still holding a reservation.
type foreignAbandon struct {
	Catalog
}

func (foreignAbandon) Abandon(res catalog.Reservation) error {
	return fmt.Errorf("%w: records %d..%d", catalog.ErrForeignReservation,
		res.FirstRecord, res.FirstRecord+uint64(res.RecordCount)) //nolint:gosec // small and positive
}

// Slots in a catalog this node no longer has are not a hole it can do anything
// about, and dressing one up as "every node stops reading there" buries the
// failure that actually needs looking at.
func TestAbandonOnAForeignCatalogReportsOnlyTheCause(t *testing.T) {
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog: foreignAbandon{Catalog: newStore(t, 8)},
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	cause := errors.New("mkfs.erofs: boom")

	err := i.abandon(catalog.Reservation{RecordCount: 2}, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("abandon() = %v, want it to carry the cause", err)
	}

	if strings.Contains(err.Error(), "permanent hole") {
		t.Errorf("abandon() = %v, want no hole report for a foreign catalog", err)
	}

	if err.Error() != cause.Error() {
		t.Errorf("abandon() = %q, want exactly the cause %q", err, cause)
	}
}

// A catalog that still owns the reservation and fails anyway is the case the
// hole report exists for.
type failedAbandon struct {
	Catalog
}

func (failedAbandon) Abandon(catalog.Reservation) error {
	return errors.New("device gone")
}

func TestAbandonReportsAHoleWhenTheCatalogOwnsTheReservation(t *testing.T) {
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog: failedAbandon{Catalog: newStore(t, 8)},
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	cause := errors.New("mkfs.erofs: boom")

	err := i.abandon(catalog.Reservation{RecordCount: 2}, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("abandon() = %v, want it to carry the cause", err)
	}

	if !strings.Contains(err.Error(), "permanent hole") {
		t.Errorf("abandon() = %v, want the hole report", err)
	}
}
