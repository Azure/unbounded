// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// rangeGetter is the subset of the SDK client a ranged read needs.
type rangeGetter interface {
	Get(ctx context.Context, request racersdk.Request, options ...racersdk.ReadOptions) (*racersdk.Object, error)
}

// rangedBody reads one pinned object version as consecutive byte ranges,
// several at a time. A range that fails mid-transfer resumes at its first
// unwritten byte instead of restarting the whole object, up to retries times.
// Bytes may be written out of order, so it suits only discard-style sinks such
// as /dev/null, not verification.
type rangedBody struct {
	ctx        context.Context
	get        func(ctx context.Context, offset, length int64) (io.WriterTo, func() error, error)
	size       int64
	rangeBytes int64
	parallel   int
	retries    int
	retryDelay time.Duration
	resumes    atomic.Int64
	onResume   func()
	// rotate shifts which range each reader starts with, so many nodes
	// reading the same object do not all request the same pages at once.
	rotate int64
}

func (b *rangedBody) Read([]byte) (int, error) {
	return 0, errors.New("ranged body supports only WriteTo")
}

func (b *rangedBody) Close() error { return nil }

// WriteTo copies every range into w. Concurrent writes to w must be safe.
func (b *rangedBody) WriteTo(w io.Writer) (int64, error) {
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()

	next := atomic.Int64{}

	var (
		total    atomic.Int64
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)

	fail := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}

	for range max(1, b.parallel) {
		wg.Go(func() {
			ranges := (b.size + b.rangeBytes - 1) / b.rangeBytes
			for {
				index := next.Add(1) - 1
				if index >= ranges || ctx.Err() != nil {
					return
				}

				start := ((index + b.rotate) % ranges) * b.rangeBytes

				end := min(start+b.rangeBytes, b.size)
				if err := b.copyRange(ctx, w, start, end, &total); err != nil {
					fail(err)

					return
				}
			}
		})
	}

	wg.Wait()

	if firstErr != nil {
		return total.Load(), firstErr
	}

	if err := b.ctx.Err(); err != nil {
		return total.Load(), err
	}

	return total.Load(), nil
}

func (b *rangedBody) copyRange(ctx context.Context, w io.Writer, start, end int64, total *atomic.Int64) error {
	offset := start

	for attempt := 0; ; attempt++ {
		body, closeBody, err := b.get(ctx, offset, end-offset)
		if err == nil {
			var n int64

			n, err = body.WriteTo(w)
			_ = closeBody() //nolint:errcheck // The copy result decides the outcome.
			offset += n
			total.Add(n)

			if err == nil && offset != end {
				err = fmt.Errorf("range [%d,%d) ended at %d: %w", start, end, offset, io.ErrUnexpectedEOF)
			}
		}

		if err == nil {
			return nil
		}

		if ctx.Err() != nil || attempt >= b.retries {
			return err
		}

		b.resumes.Add(1)

		if b.onResume != nil {
			b.onResume()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.retryDelay):
		}
	}
}

// sdkRangeGet adapts the SDK client to rangedBody, pinning every range to etag.
func sdkRangeGet(client rangeGetter, key racersdk.Key, etag string) func(context.Context, int64, int64) (io.WriterTo, func() error, error) {
	return func(ctx context.Context, offset, length int64) (io.WriterTo, func() error, error) {
		value, err := client.Get(ctx, racersdk.Request{Key: key}, racersdk.ReadOptions{Offset: offset, Length: length, ETag: etag})
		if err != nil {
			return nil, nil, err
		}

		return value, value.Close, nil
	}
}
