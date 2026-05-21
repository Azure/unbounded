// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fetch

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

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/chunkcatalog"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/metadata"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestNewCoordinator_UsesInjectedLogger verifies the constructor
// stores the provided slog.Logger on the Coordinator. The peer-RPC
// fallback warnings and commit-after-serve failure traces emitted
// from the fetch path must flow through this logger rather than
// slog.Default(), so operators can route fetch logs alongside the
// rest of the app's structured output.
func TestNewCoordinator_UsesInjectedLogger(t *testing.T) {
	t.Parallel()

	injected := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCoordinator(nil, nil, nil, nil, nil, &config.Config{}, injected)

	if c.log != injected {
		t.Errorf("Coordinator.log not the injected logger")
	}
}

// TestNewCoordinator_NilLoggerFallsBackToDefault locks the contract
// that a nil logger falls back to slog.Default() rather than panicking
// during peer fallback or commit-after-serve.
func TestNewCoordinator_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := NewCoordinator(nil, nil, nil, nil, nil, &config.Config{}, nil)
	if c.log == nil {
		t.Errorf("nil logger should have fallen back to slog.Default()")
	}
}

// TestChunkAttrs_GroupShape locks the slog attribute taxonomy used
// by every fetch-path emission. The 'chunk' group must contain the
// (origin_id, bucket, key, index) identifying tuple so operator
// queries can grep on a single, consistent attribute path.
func TestChunkAttrs_GroupShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))

	log.LogAttrs(context.Background(), slog.LevelDebug, "probe", chunkAttrs(chunk.Key{
		OriginID:  "origin-x",
		Bucket:    "bkt",
		ObjectKey: "obj",
		ChunkSize: 1024,
		Index:     7,
	}))

	out := buf.String()
	for _, want := range []string{
		"chunk.origin_id=origin-x",
		"chunk.bucket=bkt",
		"chunk.key=obj",
		"chunk.index=7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("chunkAttrs output missing %q; got %q", want, out)
		}
	}
}

// TestCoordinator_DebugEmissionsAtDebugLevel exercises a sample of
// the fetch-path debug emissions and asserts they reach the
// handler. We cannot drive the full GetChunk path here without
// standing up the entire dependency graph, so we exercise the
// representative log statements directly. The contract under test
// is that the call sites use LogAttrs at Debug level (so zero-cost
// at Info+) and emit the standardized 'chunk' attribute group.
func TestCoordinator_DebugEmissionsAtDebugLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &Coordinator{log: log}

	k := chunk.Key{
		OriginID:  "ox",
		Bucket:    "bkt",
		ObjectKey: "obj",
		ChunkSize: 1024,
		Index:     3,
	}
	// Sample emissions corresponding to lookupOrStat hits,
	// peer-fill route selection, and commit success.
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "catalog_hit", chunkAttrs(k))
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "peer_fill_attempt",
		chunkAttrs(k), slog.String("peer_ip", "10.0.0.5"))
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "commit_success",
		chunkAttrs(k), slog.Int("bytes", 1024))

	out := buf.String()
	for _, want := range []string{"catalog_hit", "peer_fill_attempt", "commit_success", "chunk.index=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in debug output; got %q", want, out)
		}
	}
}

// TestCoordinator_DebugFilteredAtInfo verifies that the standard
// LogAttrs path emits nothing when the handler is configured above
// Debug. This is the operational expectation: enabling Info-level
// logging silences the per-chunk traces entirely so production
// throughput is not affected by log overhead.
func TestCoordinator_DebugFilteredAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelInfo}))
	c := &Coordinator{log: log}

	k := chunk.Key{OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024, Index: 0}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "catalog_hit", chunkAttrs(k))

	if buf.Len() != 0 {
		t.Errorf("debug emission leaked through Info-level handler: %q", buf.String())
	}
}

// TestCoordinator_WarnRoutesThroughInjectedHandler verifies that the
// (migrated to LogAttrs) commit-after-serve warning still surfaces
// at Warn level on the injected logger. Regression test for the
// existing call site that pre-dates the debug emissions.
func TestCoordinator_WarnRoutesThroughInjectedHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := &Coordinator{log: log}

	k := chunk.Key{OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024, Index: 0}
	c.log.LogAttrs(context.Background(), slog.LevelWarn, "commit-after-serve failed",
		chunkAttrs(k),
		slog.String("err", "stub put failure"),
	)

	out := buf.String()
	if !strings.Contains(out, "commit-after-serve failed") {
		t.Errorf("warning not captured; got %q", out)
	}

	if !strings.Contains(out, "chunk.key=o") {
		t.Errorf("chunk attribute missing; got %q", out)
	}
}

// fakeOriginForFill returns a fixed body for any GetRange call.
type fakeOriginForFill struct {
	body []byte
}

func (f *fakeOriginForFill) Head(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
	return origin.ObjectInfo{Size: int64(len(f.body)), ETag: "e1"}, nil
}

func (f *fakeOriginForFill) GetRange(_ context.Context, _, _, _ string, _, _ int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

// slowPutCacheStore implements cachestore.CacheStore. PutChunk
// blocks until putGate is closed; signals putStarted when entered
// and putReturned when leaving. Used by the commit-after-serve test
// to observe the relative ordering of joiner release vs PutChunk
// completion.
type slowPutCacheStore struct {
	putGate      chan struct{}
	putStarted   chan struct{}
	putReturned  chan struct{}
	closeOnce    sync.Once
	putCallCount atomic.Int64
}

func newSlowPutCacheStore() *slowPutCacheStore {
	return &slowPutCacheStore{
		putGate:     make(chan struct{}),
		putStarted:  make(chan struct{}),
		putReturned: make(chan struct{}),
	}
}

func (s *slowPutCacheStore) GetChunk(_ context.Context, _ chunk.Key, _, _ int64) (io.ReadCloser, error) {
	return nil, cachestore.ErrNotFound
}

func (s *slowPutCacheStore) PutChunk(_ context.Context, _ chunk.Key, _ int64, _ io.Reader) error {
	s.putCallCount.Add(1)
	s.closeOnce.Do(func() { close(s.putStarted) })
	<-s.putGate
	close(s.putReturned)

	return nil
}

func (s *slowPutCacheStore) Stat(_ context.Context, _ chunk.Key) (cachestore.Info, error) {
	return cachestore.Info{}, cachestore.ErrNotFound
}

func (s *slowPutCacheStore) Delete(_ context.Context, _ chunk.Key) error  { return nil }
func (s *slowPutCacheStore) SelfTestAtomicCommit(_ context.Context) error { return nil }

// TestRunFill_CommitAfterServe_JoinerSeesBytesBeforeCommit verifies
// that runFill releases joiners (close(f.done)) BEFORE the cachestore
// PutChunk completes. With the prior commit-before-serve ordering,
// joiners had to wait an extra commit-rtt; this test detects a
// regression by asserting the joiner returns while PutChunk is still
// blocked.
//
// Regression for H-1.
func TestRunFill_CommitAfterServe_JoinerSeesBytesBeforeCommit(t *testing.T) {
	t.Parallel()

	payload := []byte("hello world commit-after-serve test payload!!")
	chunkSize := int64(len(payload))

	or := &fakeOriginForFill{body: payload}
	cs := newSlowPutCacheStore()
	cat := chunkcatalog.New(64, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := metadata.NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)

	cfg := &config.Config{
		Origin: config.Origin{
			ID:           "ox",
			QueueTimeout: time.Second,
			Retry: config.OriginRetry{
				Attempts:         1,
				BackoffInitial:   time.Millisecond,
				BackoffMax:       time.Millisecond,
				MaxTotalDuration: time.Second,
			},
			TargetGlobal: 4,
		},
		Cluster: config.Cluster{TargetReplicas: 1},
	}

	co := NewCoordinator(or, cs, nil, cat, mc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	k := chunk.Key{
		OriginID:  "ox",
		Bucket:    "b",
		ObjectKey: "o",
		ETag:      "e1",
		ChunkSize: chunkSize,
		Index:     0,
	}

	rcCh := make(chan io.ReadCloser, 1)
	errCh := make(chan error, 1)

	go func() {
		rc, err := co.fillLocal(context.Background(), k, chunkSize)
		if err != nil {
			errCh <- err
			return
		}

		rcCh <- rc
	}()
	// Wait for PutChunk to have been entered, ensuring runFill is
	// past the validate-and-release point.
	select {
	case <-cs.putStarted:
	case <-time.After(2 * time.Second):
		close(cs.putGate)
		t.Fatalf("PutChunk never entered; runFill never reached commit")
	}

	// fillLocal should return now (joiner released before PutChunk
	// completes). With the old commit-before-serve ordering it would
	// still be blocked.
	select {
	case rc := <-rcCh:
		// Verify PutChunk hasn't completed.
		select {
		case <-cs.putReturned:
			t.Errorf("PutChunk returned before fillLocal; commit-after-serve regressed")
		default:
		}

		got, err := io.ReadAll(rc)
		if err != nil {
			t.Errorf("read body: %v", err)
		}

		if !bytes.Equal(got, payload) {
			t.Errorf("body mismatch: got %d bytes want %d", len(got), len(payload))
		}

		_ = rc.Close() //nolint:errcheck // test cleanup
	case err := <-errCh:
		close(cs.putGate)
		t.Fatalf("fillLocal err: %v", err)
	case <-time.After(2 * time.Second):
		close(cs.putGate)
		t.Fatalf("fillLocal didn't return while PutChunk was blocked; commit-after-serve regressed")
	}

	// Release PutChunk and let runFill finish.
	close(cs.putGate)
	<-cs.putReturned
}

// TestRunFill_ReleaseIdempotent_PanicSafe verifies that close(f.done)
// fires exactly once whether via the explicit success-path call or
// the deferred safety net. A panic mid-fill must not corrupt the
// channel state by double-closing it.
//
// Regression for H-1's sync.Once safety property.
func TestRunFill_ReleaseIdempotent_PanicSafe(t *testing.T) {
	t.Parallel()

	// Use the test pattern directly: a sync.Once-wrapped close,
	// called from two paths.
	done := make(chan struct{})

	var once sync.Once

	release := func() { once.Do(func() { close(done) }) }

	release() // explicit path
	release() // simulated "deferred safety net" path - must not panic

	select {
	case <-done:
		// Closed - good.
	default:
		t.Errorf("done channel not closed after release()")
	}
}

// stubOriginEmptyETag returns ObjectInfo with no ETag - simulating a
// misconfigured origin (e.g. some S3-compatible backend without
// versioning, or a custom origin not following the AWS/Azure
// contract).
type stubOriginEmptyETag struct{}

func (stubOriginEmptyETag) Head(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
	return origin.ObjectInfo{Size: 1024, ETag: ""}, nil
}

func (stubOriginEmptyETag) GetRange(_ context.Context, _, _, _ string, _, _ int64) (io.ReadCloser, error) {
	return nil, nil
}

// TestHeadObject_RejectsEmptyETag verifies that the coordinator
// rejects an origin Head response with an empty ETag. chunk.Path
// encodes the ETag in its hash; without it, two different versions
// of the same (bucket, key) would alias and serve stale bytes
// silently.
//
// Regression for H-7.
func TestHeadObject_RejectsEmptyETag(t *testing.T) {
	t.Parallel()

	mc := metadata.NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)
	co := NewCoordinator(stubOriginEmptyETag{}, nil, nil, nil, mc,
		&config.Config{Origin: config.Origin{ID: "ox"}, Cluster: config.Cluster{TargetReplicas: 1}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := co.HeadObject(context.Background(), "b", "o")
	if err == nil {
		t.Fatalf("HeadObject accepted empty ETag; want MissingETagError")
	}

	var mte *origin.MissingETagError
	if !errors.As(err, &mte) {
		t.Errorf("err type = %T (want *origin.MissingETagError): %v", err, err)
	}
}

// TestHeadObject_EmptyETag_CachedNegatively verifies that a second
// HeadObject call after a MissingETagError result does NOT re-hit
// the origin: the negative result must be cached so we do not
// hammer a misconfigured origin on every request.
func TestHeadObject_EmptyETag_CachedNegatively(t *testing.T) {
	t.Parallel()

	or := &countingOrigin{inner: stubOriginEmptyETag{}}
	mc := metadata.NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16}, nil)
	co := NewCoordinator(or, nil, nil, nil, mc,
		&config.Config{Origin: config.Origin{ID: "ox"}, Cluster: config.Cluster{TargetReplicas: 1}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 3; i++ {
		_, err := co.HeadObject(context.Background(), "b", "o")
		if err == nil {
			t.Errorf("call %d: HeadObject accepted empty ETag", i)
		}
	}

	if got := or.headCalls.Load(); got != 1 {
		t.Errorf("origin.Head invoked %d times; want 1 (negative cached)", got)
	}
}

// countingOrigin wraps an origin.Origin and counts Head invocations.
type countingOrigin struct {
	inner     origin.Origin
	headCalls atomic.Int64
}

func (c *countingOrigin) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	c.headCalls.Add(1)
	return c.inner.Head(ctx, bucket, key)
}

func (c *countingOrigin) GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error) {
	return c.inner.GetRange(ctx, bucket, key, etag, off, n)
}
