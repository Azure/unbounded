// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Tests for containerdstore.Store run against an in-memory fake
// content store so they execute on darwin without a real containerd.

package containerdstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	cerrdefs "github.com/containerd/errdefs"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// fakeStore is a minimal content.Store backed by an in-memory map.
// Tracks live writers so we can exercise Writer/Commit/Abort paths.
type fakeStore struct {
	mu              sync.Mutex
	blobs           map[godigest.Digest][]byte
	writers         map[string]*fakeWriter // ref -> writer
	failInfo        godigest.Digest        // if non-empty, Info returns failInfoErr
	failInfoErr     error
	failReaderAt    godigest.Digest // if non-empty, ReaderAt returns failReaderAtErr
	failReaderAtErr error
	failWriterErr   error
}

func newFake() *fakeStore {
	return &fakeStore{
		blobs:   map[godigest.Digest][]byte{},
		writers: map[string]*fakeWriter{},
	}
}

func (f *fakeStore) put(d godigest.Digest, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.blobs[d] = b
}

func (f *fakeStore) Info(_ context.Context, dgst godigest.Digest) (content.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failInfo != "" && dgst == f.failInfo {
		return content.Info{}, f.failInfoErr
	}

	b, ok := f.blobs[dgst]
	if !ok {
		return content.Info{}, cerrdefs.ErrNotFound
	}

	return content.Info{Digest: dgst, Size: int64(len(b)), CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}

func (f *fakeStore) Update(_ context.Context, _ content.Info, _ ...string) (content.Info, error) {
	panic("fakeStore.Update not implemented")
}

func (f *fakeStore) Walk(_ context.Context, fn content.WalkFunc, _ ...string) error {
	f.mu.Lock()

	keys := make([]godigest.Digest, 0, len(f.blobs))
	for d := range f.blobs {
		keys = append(keys, d)
	}

	sizes := map[godigest.Digest]int64{}
	for _, d := range keys {
		sizes[d] = int64(len(f.blobs[d]))
	}
	f.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	for _, d := range keys {
		if err := fn(content.Info{Digest: d, Size: sizes[d]}); err != nil {
			return err
		}
	}

	return nil
}

func (f *fakeStore) Delete(_ context.Context, _ godigest.Digest) error {
	panic("fakeStore.Delete not implemented")
}

func (f *fakeStore) ReaderAt(_ context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failReaderAt != "" && desc.Digest == f.failReaderAt {
		return nil, f.failReaderAtErr
	}

	b, ok := f.blobs[desc.Digest]
	if !ok {
		return nil, cerrdefs.ErrNotFound
	}

	return &fakeReaderAt{data: b}, nil
}

func (f *fakeStore) Status(_ context.Context, ref string) (content.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.writers[ref]; !ok {
		return content.Status{}, cerrdefs.ErrNotFound
	}

	return content.Status{Ref: ref}, nil
}

func (f *fakeStore) ListStatuses(_ context.Context, _ ...string) ([]content.Status, error) {
	return nil, nil
}

func (f *fakeStore) Abort(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.writers[ref]; !ok {
		return cerrdefs.ErrNotFound
	}

	delete(f.writers, ref)

	return nil
}

func (f *fakeStore) Writer(_ context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	if f.failWriterErr != nil {
		return nil, f.failWriterErr
	}

	wopts := content.WriterOpts{}
	for _, o := range opts {
		if err := o(&wopts); err != nil {
			return nil, err
		}
	}

	if wopts.Ref == "" {
		return nil, errors.New("fakeStore.Writer: ref required")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	w := &fakeWriter{ref: wopts.Ref, expected: wopts.Desc.Digest, store: f}
	f.writers[wopts.Ref] = w

	return w, nil
}

type fakeWriter struct {
	ref      string
	expected godigest.Digest
	store    *fakeStore
	buf      bytes.Buffer
	closed   bool
}

func (w *fakeWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *fakeWriter) Close() error                { w.closed = true; return nil }
func (w *fakeWriter) Digest() godigest.Digest     { return godigest.FromBytes(w.buf.Bytes()) }
func (w *fakeWriter) Status() (content.Status, error) {
	return content.Status{Ref: w.ref}, nil
}
func (w *fakeWriter) Truncate(_ int64) error { return nil }

func (w *fakeWriter) Commit(_ context.Context, _ int64, expected godigest.Digest, _ ...content.Opt) error {
	got := godigest.FromBytes(w.buf.Bytes())

	want := expected
	if want == "" {
		want = w.expected
	}

	if want != "" && got != want {
		return errors.New("fakeWriter.Commit: digest mismatch")
	}

	w.store.mu.Lock()
	defer w.store.mu.Unlock()

	if _, ok := w.store.blobs[got]; ok {
		delete(w.store.writers, w.ref)
		return cerrdefs.ErrAlreadyExists
	}

	w.store.blobs[got] = append([]byte(nil), w.buf.Bytes()...)
	delete(w.store.writers, w.ref)

	return nil
}

type fakeReaderAt struct{ data []byte }

func (r *fakeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}

	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}
func (r *fakeReaderAt) Close() error { return nil }
func (r *fakeReaderAt) Size() int64  { return int64(len(r.data)) }

var _ content.Store = (*fakeStore)(nil)

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func mustDigest(t *testing.T, payload []byte) gdigest.Digest {
	t.Helper()

	d, err := gdigest.Parse(godigest.FromBytes(payload).String())
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}

	return d
}

// TestStore_HasAndOpen exercises the read paths against present and
// absent digests, plus the "backend error" path that must surface a
// non-nil error rather than rolling into has=false.
func TestStore_HasAndOpen(t *testing.T) {
	cs := newFake()
	payload := []byte("hello-from-containerd")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)
	s := New(cs)

	d := mustDigest(t, payload)

	got, err := s.Has(context.Background(), d)
	if err != nil || !got {
		t.Fatalf("Has(present) = %v, %v; want true, nil", got, err)
	}

	r, n, err := s.Open(context.Background(), d)
	if err != nil {
		t.Fatalf("Open(present) err = %v", err)
	}

	if n != int64(len(payload)) {
		t.Errorf("Open size = %d, want %d", n, len(payload))
	}

	read, _ := io.ReadAll(r)
	if !bytes.Equal(read, payload) {
		t.Errorf("Open bytes = %q, want %q", read, payload)
	}

	r.Close()

	absent := mustDigest(t, []byte("missing"))

	got, err = s.Has(context.Background(), absent)
	if err != nil || got {
		t.Errorf("Has(absent) = %v, %v; want false, nil", got, err)
	}

	_, _, err = s.Open(context.Background(), absent)

	var nf *ifaces.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("Open(absent) err = %v, want *ErrNotFound", err)
	}
}

// TestStore_HasPropagatesBackendError covers the plan-mandated
// "containerd unreachable" contract: a non-NotFound error from the
// backend MUST surface so callers do not advertise stale availability.
// Has uses ReaderAt (not Info) to enforce openability, so the failure
// is injected at the ReaderAt boundary.
func TestStore_HasPropagatesBackendError(t *testing.T) {
	cs := &flakyReaderStore{fakeStore: newFake(), failErr: errors.New("containerd: simulated transport failure")}
	payload := []byte("triggers")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)

	s := New(cs)
	d := mustDigest(t, payload)

	got, err := s.Has(context.Background(), d)
	if err == nil {
		t.Fatal("Has returned nil err on backend failure; expected propagation")
	}

	if got {
		t.Error("Has returned true on backend failure; expected false")
	}
}

// TestStore_WriterCommit exercises the happy ingest path: stream bytes,
// Commit, then verify the blob is readable via Has/Open.
func TestStore_WriterCommit(t *testing.T) {
	cs := newFake()
	s := New(cs)
	payload := []byte("ingested-via-gantry")
	d := mustDigest(t, payload)

	w, err := s.Writer(context.Background(), d)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}

	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	has, err := s.Has(context.Background(), d)
	if err != nil || !has {
		t.Errorf("Has after commit = %v, %v; want true, nil", has, err)
	}
}

func TestStore_WriterAlreadyExistsRechecksPresence(t *testing.T) {
	cs := newFake()
	payload := []byte("already-committed-by-containerd")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)
	cs.failWriterErr = cerrdefs.ErrAlreadyExists
	s := New(cs)
	d := mustDigest(t, payload)

	w, err := s.Writer(context.Background(), d)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}

	if n, err := w.Write([]byte("ignored-race-body")); err != nil || n != len("ignored-race-body") {
		t.Fatalf("Write = %d, %v", n, err)
	}

	if err := w.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestStore_WriterAlreadyExistsButMissingIsUnavailable(t *testing.T) {
	cs := newFake()
	cs.failWriterErr = cerrdefs.ErrAlreadyExists
	s := New(cs)
	d := mustDigest(t, []byte("not-actually-present"))

	_, err := s.Writer(context.Background(), d)

	var unavailable *ifaces.ErrUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("Writer err = %v (%T), want *ifaces.ErrUnavailable", err, err)
	}
}

// TestStore_WriterCommitDigestMismatch ensures a digest mismatch at
// Commit surfaces as an error and does not stage the bytes.
func TestStore_WriterCommitDigestMismatch(t *testing.T) {
	cs := newFake()
	s := New(cs)
	declared := mustDigest(t, []byte("declared-bytes"))
	// Write different bytes than declared.
	w, err := s.Writer(context.Background(), declared)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}

	if _, err := w.Write([]byte("totally-other-bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(context.Background()); err == nil {
		t.Fatal("Commit returned nil err on digest mismatch; want non-nil")
	} else if !strings.Contains(err.Error(), "Commit") && !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("Commit err = %v, want digest-mismatch flavour", err)
	}

	has, err := s.Has(context.Background(), declared)
	if err != nil || has {
		t.Errorf("Has after failed commit = %v, %v; want false, nil", has, err)
	}
}

// TestStore_WriterAbortIdempotent verifies Abort releases the ingest
// slot and is safe to call multiple times.
func TestStore_WriterAbortIdempotent(t *testing.T) {
	cs := newFake()
	s := New(cs)
	d := mustDigest(t, []byte("never-committed"))

	w, err := s.Writer(context.Background(), d)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}

	if err := w.Abort(context.Background()); err != nil {
		t.Fatalf("Abort #1: %v", err)
	}

	if err := w.Abort(context.Background()); err != nil {
		t.Errorf("Abort #2 (idempotent) returned err: %v", err)
	}

	has, err := s.Has(context.Background(), d)
	if err != nil || has {
		t.Errorf("Has after abort = %v, %v; want false, nil", has, err)
	}
}

// TestStore_Inventory enumerates the content store via Walk and skips
// non-sha256 algorithms.
func TestStore_Inventory(t *testing.T) {
	cs := newFake()
	payloads := [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}
	want := map[string]struct{}{}

	for _, p := range payloads {
		gd := godigest.FromBytes(p)
		cs.put(gd, p)
		want[gd.String()] = struct{}{}
	}
	// Plant a non-sha256 entry that should be skipped.
	const sha512Hex = "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	cs.put(godigest.Digest("sha512:"+sha512Hex), []byte("ignored"))

	s := New(cs)

	got, err := s.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	if len(got) != len(want) {
		t.Errorf("Inventory length = %d, want %d (non-sha256 skipped)", len(got), len(want))
	}

	for _, d := range got {
		if _, ok := want[d.String()]; !ok {
			t.Errorf("unexpected digest %s in Inventory", d)
		}
	}
}

func TestStore_InventoryRequiresOpenableContent(t *testing.T) {
	cs := &flakyReaderStore{fakeStore: newFake(), failErr: cerrdefs.ErrNotFound}
	payload := []byte("walk-sees-but-open-misses")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)

	s := New(cs)

	got, err := s.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("Inventory returned %d digests, want 0 when ReaderAt reports not found", len(got))
	}
}

func TestStore_InventoryUnavailableOnOpenProbeError(t *testing.T) {
	cs := &flakyReaderStore{fakeStore: newFake(), failErr: errors.New("reader transport down")}
	payload := []byte("walk-sees-but-open-errors")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)

	s := New(cs)
	_, err := s.Inventory(context.Background())

	var unavailable *ifaces.ErrUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("Inventory err = %v (%T), want *ifaces.ErrUnavailable", err, err)
	}
}

// TestStore_HasUnavailableMappedToErrUnavailable verifies the // contract that non-NotFound backend errors surface as
// *ifaces.ErrUnavailable so transfer/coord callers can distinguish
// "containerd is sick" from "definitively absent". Without this
// callers would treat a transport failure as a cache miss and
// either advertise stale absence or serve a spurious 404. Has now
// probes via ReaderAt (openability), so the simulated failure is
// injected at that layer.
func TestStore_HasUnavailableMappedToErrUnavailable(t *testing.T) {
	cs := &flakyReaderStore{fakeStore: newFake(), failErr: errors.New("containerd: simulated transport failure")}
	payload := []byte("triggers-unavailable")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)

	s := New(cs)
	d := mustDigest(t, payload)

	_, err := s.Has(context.Background(), d)
	if err == nil {
		t.Fatal("Has returned nil err on backend failure")
	}

	var eun *ifaces.ErrUnavailable
	if !errors.As(err, &eun) {
		t.Errorf("Has err = %v (%T); want *ifaces.ErrUnavailable", err, err)
	}
}

// TestStore_OpenUnavailableMappedToErrUnavailable mirrors the Has
// test for ReaderAt failures. ErrNotFound is a separate path
// (ErrNotFound, asserted elsewhere) and MUST NOT collapse with the
// unavailable case.
func TestStore_OpenUnavailableMappedToErrUnavailable(t *testing.T) {
	cs := &flakyReaderStore{fakeStore: newFake(), failErr: errors.New("ReaderAt: simulated I/O error")}
	payload := []byte("triggers-unavailable-open")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)

	s := New(cs)
	d := mustDigest(t, payload)

	_, _, err := s.Open(context.Background(), d)
	if err == nil {
		t.Fatal("Open returned nil err on backend failure")
	}

	var eun *ifaces.ErrUnavailable
	if !errors.As(err, &eun) {
		t.Errorf("Open err = %v (%T); want *ifaces.ErrUnavailable", err, err)
	}

	var enf *ifaces.ErrNotFound
	if errors.As(err, &enf) {
		t.Error("Open err collapsed to ErrNotFound; expected ErrUnavailable")
	}
}

func TestStore_ContextErrorsAreNotMappedToUnavailable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			cs := &flakyReaderStore{fakeStore: newFake(), failErr: test.err}
			payload := []byte("caller-context-" + test.name)
			cs.put(godigest.FromBytes(payload), payload)

			var ctx context.Context

			if errors.Is(test.err, context.DeadlineExceeded) {
				var cancel context.CancelFunc

				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
			} else {
				var cancel context.CancelFunc

				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}

			s := New(cs)
			d := mustDigest(t, payload)

			if _, err := s.Has(ctx, d); !errors.Is(err, test.err) {
				t.Fatalf("Has err = %v, want %v", err, test.err)
			}

			if _, _, err := s.Open(ctx, d); !errors.Is(err, test.err) {
				t.Fatalf("Open err = %v, want %v", err, test.err)
			}
		})
	}
}

// flakyReaderStore wraps fakeStore and forces ReaderAt to fail with a
// non-NotFound error so we can exercise the unavailable path.
type flakyReaderStore struct {
	*fakeStore
	failErr error
}

func (f *flakyReaderStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, f.failErr
}

// TestStore_MetricsHooksFireForHitMissUnavailable wires WithMetrics
// and verifies the four counters expected by phase9Metrics increment
// as documented.
func TestStore_MetricsHooksFireForHitMissUnavailable(t *testing.T) {
	cs := newFake()
	present := []byte("present-bytes")
	pgd := godigest.FromBytes(present)
	cs.put(pgd, present)

	absent := mustDigest(t, []byte("absent-bytes"))

	var hits, misses, unav, openErr int

	s := New(cs, WithMetrics(MetricsHooks{
		OnHit:         func() { hits++ },
		OnMiss:        func() { misses++ },
		OnUnavailable: func() { unav++ },
		OnOpenError:   func() { openErr++ },
	}))
	pdig := mustDigest(t, present)

	if has, err := s.Has(context.Background(), pdig); !has || err != nil {
		t.Fatalf("Has(present) = %v, %v", has, err)
	}

	if has, err := s.Has(context.Background(), absent); has || err != nil {
		t.Fatalf("Has(absent) = %v, %v", has, err)
	}

	cs.failReaderAt = pgd
	cs.failReaderAtErr = errors.New("transport sad")

	if _, err := s.Has(context.Background(), pdig); err == nil {
		t.Fatal("Has returned nil on forced failure")
	}

	if hits != 1 || misses != 1 || unav != 1 {
		t.Errorf("hooks: hits=%d misses=%d unav=%d openErr=%d (want 1,1,1,0)", hits, misses, unav, openErr)
	}
}

// TestStore_RememberMediaTypeRoundTripAndCap verifies the descriptor
// index returns the stored mediaType from Descriptor and that the
// cap-based eviction bound holds. Eviction is random; we only assert
// the size invariant.
func TestStore_RememberMediaTypeRoundTripAndCap(t *testing.T) {
	cs := newFake()
	payload := []byte("descriptor-test")
	gd := godigest.FromBytes(payload)
	cs.put(gd, payload)
	s := New(cs)
	d := mustDigest(t, payload)

	// Empty mediaType is ignored.
	s.RememberMediaType(d, "")

	if got := s.lookupMediaType(d); got != "" {
		t.Errorf("empty mediaType not ignored: got %q", got)
	}

	const mt = "application/vnd.oci.image.manifest.v1+json"
	s.RememberMediaType(d, mt)

	if got := s.LookupMediaType(d); got != mt {
		t.Errorf("LookupMediaType = %q, want %q", got, mt)
	}

	desc, err := s.Descriptor(context.Background(), d)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}

	if desc.MediaType != mt {
		t.Errorf("Descriptor.MediaType = %q, want %q", desc.MediaType, mt)
	}

	// Test cap eviction by overriding descCap to a small bound.
	s.descCap = 4

	for i := 0; i < 32; i++ {
		fake, _ := gdigest.Parse(godigest.FromBytes([]byte{byte(i), byte(i + 1)}).String())
		s.RememberMediaType(fake, "x")
	}

	if got := len(s.descIndex); got > s.descCap {
		t.Errorf("descIndex size = %d, exceeds cap %d", got, s.descCap)
	}
}
