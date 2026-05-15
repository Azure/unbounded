// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metadata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestLookupOrFetch_TransientErrorNotReplayed verifies that after the
// leader of a singleflight fetch returns a transient (non-cached)
// error, a subsequent call to LookupOrFetch invokes fetch again
// rather than silently replaying the cached error.
//
// Regression test for the defer-order race: with `close(done)` before
// `Delete`, a second caller arriving in the gap would land on the
// stale singleflight entry and skip fetch entirely.
func TestLookupOrFetch_TransientErrorNotReplayed(t *testing.T) {
	t.Parallel()

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)

	var calls atomic.Int64

	transientErr := errors.New("transient: try again")

	fetch := func(_ context.Context) (origin.ObjectInfo, error) {
		calls.Add(1)
		return origin.ObjectInfo{}, transientErr
	}

	// Sequential calls: each must invoke fetch, never replay.
	for i := 0; i < 5; i++ {
		_, err := c.LookupOrFetch(t.Context(), "origin", "bucket", "key", fetch)
		if !errors.Is(err, transientErr) {
			t.Fatalf("call %d: err=%v want %v", i, err, transientErr)
		}
	}

	if got := calls.Load(); got != 5 {
		t.Errorf("fetch invoked %d times, want 5 (transient errors must not be cached)", got)
	}
}

// TestLookupOrFetch_PositiveResultCached verifies positive results
// are served from the cache without re-invoking fetch.
func TestLookupOrFetch_PositiveResultCached(t *testing.T) {
	t.Parallel()

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)

	var calls atomic.Int64

	want := origin.ObjectInfo{Size: 1234, ETag: "abc"}

	fetch := func(_ context.Context) (origin.ObjectInfo, error) {
		calls.Add(1)
		return want, nil
	}

	for i := 0; i < 5; i++ {
		got, err := c.LookupOrFetch(t.Context(), "origin", "bucket", "key", fetch)
		if err != nil {
			t.Fatalf("call %d: err=%v", i, err)
		}

		if got != want {
			t.Errorf("call %d: got %+v want %+v", i, got, want)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("fetch invoked %d times, want 1 (positive results must be cached)", got)
	}
}

// TestLookupOrFetch_NotFoundCached verifies origin.ErrNotFound is
// negatively cached and replayed without re-invoking fetch.
func TestLookupOrFetch_NotFoundCached(t *testing.T) {
	t.Parallel()

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)

	var calls atomic.Int64

	fetch := func(_ context.Context) (origin.ObjectInfo, error) {
		calls.Add(1)
		return origin.ObjectInfo{}, origin.ErrNotFound
	}

	for i := 0; i < 3; i++ {
		_, err := c.LookupOrFetch(t.Context(), "origin", "bucket", "key", fetch)
		if !errors.Is(err, origin.ErrNotFound) {
			t.Fatalf("call %d: err=%v want ErrNotFound", i, err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("fetch invoked %d times, want 1 (ErrNotFound must be negatively cached)", got)
	}
}

// TestLookupOrFetch_ConcurrentJoinersCollapse verifies that
// simultaneous callers for the same key collapse to a single fetch.
func TestLookupOrFetch_ConcurrentJoinersCollapse(t *testing.T) {
	t.Parallel()

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)

	var calls atomic.Int64

	gate := make(chan struct{})
	want := origin.ObjectInfo{Size: 42}

	fetch := func(_ context.Context) (origin.ObjectInfo, error) {
		calls.Add(1)
		<-gate // pin the leader until joiners have arrived

		return want, nil
	}

	const n = 8

	var (
		wg      sync.WaitGroup
		results = make([]origin.ObjectInfo, n)
		errs    = make([]error, n)
	)

	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()

			results[i], errs[i] = c.LookupOrFetch(t.Context(), "origin", "bucket", "key", fetch)
		}(i)
	}

	time.Sleep(50 * time.Millisecond) // let everyone arrive at the singleflight
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("fetch invoked %d times, want 1 (joiners must collapse)", got)
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: err=%v", i, err)
		}

		if results[i] != want {
			t.Errorf("call %d: got %+v want %+v", i, results[i], want)
		}
	}
}

// TestMkKey_PipeCollisionResolved verifies that length-prefixed
// encoding distinguishes (origin, bucket, key) triples that
// previously aliased on the pipe-delimited concatenation.
//
// Under the old 'origin|bucket|key' shape, S3 object keys legally
// containing '|' could produce key collisions across distinct
// triples: ("a|b","c","d") and ("a","b|c","d") rendered to the
// same string. The length-prefix encoding guarantees uniqueness.
func TestMkKey_PipeCollisionResolved(t *testing.T) {
	t.Parallel()

	a := mkKey("a|b", "c", "d")
	b := mkKey("a", "b|c", "d")

	if a == b {
		t.Errorf("pipe-delimited collision: mkKey(%q,%q,%q) == mkKey(%q,%q,%q) = %q",
			"a|b", "c", "d", "a", "b|c", "d", a)
	}
}

// TestNewCache_UsesInjectedLogger locks the contract that the
// metadata cache uses the caller's logger rather than slog.Default.
func TestNewCache_UsesInjectedLogger(t *testing.T) {
	t.Parallel()

	injected := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, injected)

	if c.log != injected {
		t.Errorf("metadata.Cache.log not the injected logger")
	}
}

// TestNewCache_NilLoggerFallsBackToDefault verifies the nil-logger
// fallback so a misconfigured caller does not panic on the first
// trace emission.
func TestNewCache_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)
	if c.log == nil {
		t.Errorf("nil logger should have fallen back to slog.Default()")
	}
}

// TestLookupOrFetch_EmitsDebugTraces verifies that the metadata
// cache emits the documented debug-level emissions on the leader,
// joiner, hit, and record-result paths. The contract under test is
// the named messages and the (origin_id, bucket, key) attribute
// triple - operators rely on these for diagnosing cache-hit
// patterns.
func TestLookupOrFetch_EmitsDebugTraces(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, log)

	want := origin.ObjectInfo{Size: 42, ETag: "etag"}
	// First call: leader path + positive record.
	info, err := c.LookupOrFetch(context.Background(), "ox", "bkt", "obj",
		func(_ context.Context) (origin.ObjectInfo, error) {
			return want, nil
		})
	if err != nil || info.Size != 42 {
		t.Fatalf("LookupOrFetch leader: info=%+v err=%v", info, err)
	}
	// Second call: cache hit path. The fetch function must not run.
	_, err = c.LookupOrFetch(context.Background(), "ox", "bkt", "obj",
		func(_ context.Context) (origin.ObjectInfo, error) {
			t.Fatalf("fetch should not run on cache hit")
			return origin.ObjectInfo{}, nil
		})
	if err != nil {
		t.Fatalf("LookupOrFetch hit: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"metadata_singleflight_leader",
		"metadata_record",
		"metadata_hit",
		"bucket=bkt",
		"key=obj",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in debug output; got %q", want, out)
		}
	}
}
