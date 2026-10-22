// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// pattern returns the deterministic byte at off.
func pattern(off int64) byte {
	x := uint64(off) * 0x9E3779B97F4A7C15 //nolint:gosec // Offsets are never negative.
	return byte(x >> 56)
}

func expected(off, n, size int64) []byte {
	out := make([]byte, n)
	for i := range out {
		if o := off + int64(i); o < size {
			out[i] = pattern(o)
		}
	}

	return out
}

type openCall struct {
	off, length int64
	etag        string
}

// fakeSource serves pattern data with scripted faults.
type fakeSource struct {
	size int64
	etag string

	mu    sync.Mutex
	calls []openCall

	stats, warms atomic.Int64

	statErr error
	warmErr error
	// warmFailAfterFirst makes every Warm after the first fail.
	warmFailAfterFirst bool
	// onOpen, when set, runs for every Open with the 1-based call number.
	// A non-nil reader or error replaces the default behavior.
	onOpen func(ctx context.Context, n int, c openCall) (io.ReadCloser, error)
}

func newFake(size int64) *fakeSource {
	return &fakeSource{size: size, etag: `"v1"`}
}

func (f *fakeSource) md() racersdk.Metadata {
	return racersdk.Metadata{Size: f.size, ETag: f.etag, ExpiresAt: time.Now().Add(time.Hour)}
}

func (f *fakeSource) Stat(context.Context) (racersdk.Metadata, error) {
	f.stats.Add(1)

	if f.statErr != nil {
		return racersdk.Metadata{}, f.statErr
	}

	return f.md(), nil
}

func (f *fakeSource) Warm(_ context.Context, ttl time.Duration, etag string) (racersdk.WarmResult, error) {
	if f.warms.Add(1) > 1 && f.warmFailAfterFirst {
		return racersdk.WarmResult{}, racersdk.ErrUnavailable
	}

	if f.warmErr != nil {
		return racersdk.WarmResult{}, f.warmErr
	}

	if etag != "" && etag != f.etag {
		return racersdk.WarmResult{}, racersdk.ErrVersionMismatch
	}

	return racersdk.WarmResult{Metadata: f.md(), RetainUntil: time.Now().Add(ttl)}, nil
}

func (f *fakeSource) Open(ctx context.Context, off, length int64, etag string) (io.ReadCloser, error) {
	c := openCall{off: off, length: length, etag: etag}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	n := len(f.calls)
	f.mu.Unlock()

	if f.onOpen != nil {
		r, err := f.onOpen(ctx, n, c)
		if r != nil || err != nil {
			return r, err
		}
	}

	if etag != "" && etag != f.etag {
		return nil, racersdk.ErrVersionMismatch
	}

	if off < 0 || length <= 0 || off+length > f.size {
		return nil, racersdk.ErrRangeNotSatisfiable
	}

	return &patternReader{off: off, end: off + length, md: f.md()}, nil
}

func (f *fakeSource) openCalls() []openCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]openCall(nil), f.calls...)
}

type patternReader struct {
	off, end int64
	md       racersdk.Metadata
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), r.end-r.off))
	for i := range n {
		p[i] = pattern(r.off + int64(i))
	}

	r.off += int64(n)

	return n, nil
}

func (r *patternReader) Close() error { return nil }

func (r *patternReader) Metadata() racersdk.Metadata { return r.md }

// gate blocks readers until opened.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testConfig() Config {
	return Config{ChunkSize: MinChunkSize, CacheBytes: 64 * MinChunkSize, ReadaheadMax: -1, Logger: quietLogger()}
}

func mustOpen(t *testing.T, src Source, cfg Config) *Backend {
	t.Helper()

	b, err := Open(t.Context(), src, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = b.Close() })

	return b
}

func checkRead(t *testing.T, b *Backend, off, n int64) {
	t.Helper()

	p := bytes.Repeat([]byte{0xAA}, int(n))
	if err := b.ReadAt(t.Context(), p, off); err != nil {
		t.Fatalf("ReadAt(%d, %d): %v", off, n, err)
	}

	if want := expected(off, n, b.Size()); !bytes.Equal(p, want) {
		t.Fatalf("ReadAt(%d, %d): data mismatch", off, n)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}
