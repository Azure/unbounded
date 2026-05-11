// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metadata

import (
	"context"
	"errors"
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

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16})

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

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16})

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

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16})

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

	c := NewCache(config.Metadata{TTL: time.Minute, NegativeTTL: time.Minute, MaxEntries: 16})

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
