// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
)

type commitOnlyCache struct{}

func (commitOnlyCache) Has(context.Context, digest.Digest) (bool, error) { return false, nil }
func (commitOnlyCache) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, &ifaces.ErrNotFound{}
}

func (commitOnlyCache) Writer(context.Context, digest.Digest) (ifaces.ContentWriter, error) {
	return commitOnlyWriter{}, nil
}

type commitOnlyWriter struct{}

func (commitOnlyWriter) Write(p []byte) (int, error)  { return len(p), nil }
func (commitOnlyWriter) Commit(context.Context) error { return nil }
func (commitOnlyWriter) Abort(context.Context) error  { return nil }

func TestRunOriginPull_ReopenFailurePreventsAdvertiseAndSuccess(t *testing.T) {
	body := []byte("committed-but-not-reopenable")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)
	h, _, _ := inflight.New(inflight.DefaultStalls(), nil).Start(d, ifaces.KindBlob, 0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var markPresent, successes, downstream int32

	runOriginPull(originPuller, commitOnlyCache{}, nil, logger, h, "registry.example.com", "library/test", d, ifaces.KindBlob,
		func(context.Context, digest.Digest) bool {
			atomic.AddInt32(&markPresent, 1)
			return true
		},
		func(string, int64) { atomic.AddInt32(&successes, 1) },
		func(string, string) { atomic.AddInt32(&downstream, 1) },
		leaseMetricHooks{},
	)

	if got := atomic.LoadInt32(&markPresent); got != 0 {
		t.Fatalf("markPresent calls = %d, want 0 when reopen fails", got)
	}

	if got := atomic.LoadInt32(&successes); got != 0 {
		t.Fatalf("success calls = %d, want 0 when reopen fails", got)
	}

	if got := atomic.LoadInt32(&downstream); got != 1 {
		t.Fatalf("downstream failures = %d, want 1", got)
	}
}

func TestRunOriginPull_MarkPresentFailurePreventsSuccess(t *testing.T) {
	body := []byte("commit-reopen-ok-advertise-fails")
	d := trackerDigestOf(body)
	originPuller := fakes.NewOriginPuller()
	originPuller.Put(d, body)

	cache := fakes.NewCache()
	h, _, _ := inflight.New(inflight.DefaultStalls(), nil).Start(d, ifaces.KindBlob, 0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var successes, downstream int32

	runOriginPull(originPuller, cache, nil, logger, h, "registry.example.com", "library/test", d, ifaces.KindBlob,
		func(context.Context, digest.Digest) bool { return false },
		func(string, int64) { atomic.AddInt32(&successes, 1) },
		func(string, string) { atomic.AddInt32(&downstream, 1) },
		leaseMetricHooks{},
	)

	if got := atomic.LoadInt32(&successes); got != 0 {
		t.Fatalf("success calls = %d, want 0 when mark-present fails", got)
	}

	if got := atomic.LoadInt32(&downstream); got != 1 {
		t.Fatalf("downstream failures = %d, want 1", got)
	}

	if ok, err := cache.Has(context.Background(), d); err != nil || !ok {
		t.Fatalf("cache.Has after commit = %v, %v; want true, nil", ok, err)
	}
}
