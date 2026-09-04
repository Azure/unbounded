// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest

package inttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	cachestores3 "github.com/Azure/unbounded/internal/orca/cachestore/s3"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// newCachestoreDriver builds a cachestore/s3 driver pointed at a fresh
// S3-backend bucket. Mirrors how app.go wires the production driver.
func newCachestoreDriver(ctx context.Context, t *testing.T) (*cachestores3.Driver, string) {
	t.Helper()

	bucket := pkgS3.NewBucket(ctx, t, "orca-cache")

	d, err := cachestores3.New(ctx, cachestores3.Config{
		Endpoint:     pkgS3.Endpoint(),
		Bucket:       bucket,
		Region:       pkgS3.Region(),
		AccessKey:    pkgS3.AccessKey(),
		SecretKey:    pkgS3.SecretKey(),
		UsePathStyle: true,
	}, nil)
	if err != nil {
		t.Fatalf("cachestore/s3 New: %v", err)
	}

	return d, bucket
}

// testChunkKey returns a deterministic chunk key for commit tests.
func testChunkKey() chunk.Key {
	return chunk.Key{
		OriginID:  "ox",
		Bucket:    "b",
		ObjectKey: "commit-object",
		ETag:      "etag-commit-1",
		ChunkSize: 1024,
		Index:     0,
	}
}

// TestCachestoreSelfTest_PassesAgainstS3Backend verifies the boot-time
// read-after-write self-test succeeds against the S3 backend. This is
// the gate app.go runs before serving; a regression here means orca
// would refuse to start against the backend.
func TestCachestoreSelfTest_PassesAgainstS3Backend(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	d, _ := newCachestoreDriver(ctx, t)

	if err := d.SelfTest(ctx); err != nil {
		t.Fatalf("SelfTest against S3 backend: %v", err)
	}
}

// TestCachestoreCommit_StatThenPut verifies the stat-then-put commit
// semantics end-to-end against a real S3-compatible backend:
//
//   - the first PutChunk for a key stores the bytes,
//   - a second PutChunk for the same key returns ErrCommitLost and
//     leaves the stored bytes intact (no redundant upload),
//   - the stored object is byte-exact.
func TestCachestoreCommit_StatThenPut(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	d, _ := newCachestoreDriver(ctx, t)

	k := testChunkKey()
	data := bytes.Repeat([]byte("orca-chunk-bytes!"), 4096) // ~68 KiB

	// First commit: stores.
	if err := d.PutChunk(ctx, k, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// Second commit of identical content: stat-then-put finds the
	// object already present and reports ErrCommitLost.
	err := d.PutChunk(ctx, k, int64(len(data)), bytes.NewReader(data))
	if !errors.Is(err, cachestore.ErrCommitLost) {
		t.Fatalf("second PutChunk: got %v, want ErrCommitLost", err)
	}

	// Stored bytes are byte-exact.
	rc, err := d.GetChunk(ctx, k, 0, int64(len(data)))
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	defer rc.Close() //nolint:errcheck // best-effort in test

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}

	if !bytes.Equal(got, data) {
		t.Fatalf("stored bytes mismatch: got %d, want %d", len(got), len(data))
	}
}

// TestCachestoreCommit_ConcurrentIdenticalContent simulates the
// cross-replica fill race the stat-then-put commit is designed for:
// many goroutines commit byte-identical content to the same key at
// once (the only way two writers ever target the same key, since the
// ETag is part of the path). Every commit must either store the bytes
// or report ErrCommitLost, never a hard error, and the final stored
// object must be byte-exact.
func TestCachestoreCommit_ConcurrentIdenticalContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	d, _ := newCachestoreDriver(ctx, t)

	k := testChunkKey()
	data := bytes.Repeat([]byte("race-bytes-0123456789"), 8192) // ~168 KiB

	const writers = 8

	var (
		wg        sync.WaitGroup
		stored    atomic.Int32
		lost      atomic.Int32
		start     = make(chan struct{})
		firstErr  error
		firstOnce sync.Once
	)

	for range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start // release all writers together

			switch err := d.PutChunk(ctx, k, int64(len(data)), bytes.NewReader(data)); {
			case err == nil:
				stored.Add(1)
			case errors.Is(err, cachestore.ErrCommitLost):
				lost.Add(1)
			default:
				firstOnce.Do(func() { firstErr = err })
			}
		}()
	}

	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent PutChunk hard error: %v", firstErr)
	}

	if got := stored.Load() + lost.Load(); got != writers {
		t.Fatalf("accounting mismatch: stored=%d lost=%d total=%d want %d",
			stored.Load(), lost.Load(), got, writers)
	}

	if stored.Load() == 0 {
		t.Fatal("no writer stored the chunk")
	}

	// Whoever won, the stored object is byte-exact.
	rc, err := d.GetChunk(ctx, k, 0, int64(len(data)))
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	defer rc.Close() //nolint:errcheck // best-effort in test

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}

	if !bytes.Equal(got, data) {
		t.Fatalf("stored bytes mismatch after race: got %d, want %d", len(got), len(data))
	}
}
