// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	backoffBase = 25 * time.Millisecond
	backoffMax  = time.Second
	logInterval = 10 * time.Second
	logKeys     = 64
)

// permanent errors are returned without retrying.
var permanent = []error{
	racersdk.ErrVersionMismatch, racersdk.ErrNotFound, racersdk.ErrForbidden,
	racersdk.ErrUnauthorized, racersdk.ErrRangeNotSatisfiable, racersdk.ErrInvalidRequest,
	context.Canceled, context.DeadlineExceeded, net.ErrClosed,
}

// retryable reports whether a fetch error may succeed on retry. Besides
// racersdk.ErrUnavailable and truncated streams, unclassified errors such as
// protocol violations are retried, since the attempt count bounds them.
func retryable(err error) bool {
	for _, p := range permanent {
		if errors.Is(err, p) {
			return false
		}
	}

	return true
}

// backoff returns the delay before attempt+1: 25 ms doubling per attempt,
// capped at 1 s, with jitter in [d/2, d).
func backoff(attempt int) time.Duration {
	d := backoffBase << min(attempt-1, 10)
	d = min(d, backoffMax)

	return d/2 + rand.N(d/2) //nolint:gosec // Jitter needs no cryptographic randomness.
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// logLimiter allows one log per key per logInterval.
type logLimiter struct {
	mu   sync.Mutex
	last map[string]int64
}

func (l *logLimiter) allow(key string) bool {
	now := time.Now().UnixNano()

	l.mu.Lock()
	defer l.mu.Unlock()

	if t, ok := l.last[key]; ok && now-t < int64(logInterval) {
		return false
	}

	if len(l.last) >= logKeys {
		clear(l.last)
	}

	l.last[key] = now

	return true
}

// refreshWarm renews Racer retention about every WarmTTL/2 until Close.
func (b *Backend) refreshWarm() {
	interval := max(b.cfg.WarmTTL/2, time.Millisecond)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}

		_, err := b.src.Warm(b.ctx, b.cfg.WarmTTL, b.etag)
		if err == nil || b.ctx.Err() != nil {
			continue
		}

		if b.limiter.allow("warm: " + err.Error()) {
			level := slog.LevelWarn
			if !retryable(err) {
				level = slog.LevelError
			}

			b.log.Log(b.ctx, level, "racer warm refresh failed", "etag", b.etag, "error", err)
		}
	}
}
