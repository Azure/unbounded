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

// permanent errors are returned without retrying. context.DeadlineExceeded is
// absent because the SDK's own dial timeout matches it while the backend
// context is still live; fetch stops retrying once that context ends.
var permanent = []error{
	racersdk.ErrVersionMismatch, racersdk.ErrNotFound, racersdk.ErrForbidden,
	racersdk.ErrUnauthorized, racersdk.ErrRangeNotSatisfiable, racersdk.ErrInvalidRequest,
	context.Canceled, net.ErrClosed,
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

// minWarmDelay is the shortest delay between warm renewals, so the loop never
// spins.
const minWarmDelay = time.Millisecond

// nextWarm returns the delay before the next warm renewal.
//
// With a valid grant, the delay is half the smaller of the time left and
// WarmTTL. Without one, it is WarmTTL/2. The minWarmDelay floor prevents a
// busy loop; with less than 2*minWarmDelay left, it can schedule at or after
// expiry.
func nextWarm(now, retainUntil time.Time, ttl time.Duration) time.Duration {
	d := ttl / 2
	if left := retainUntil.Sub(now); !retainUntil.IsZero() && left > 0 {
		d = min(d, left/2)
	}

	return max(d, minWarmDelay)
}

// refreshWarm renews Racer retention until Close. retainUntil is the
// deadline granted at Open, or zero if Racer refused retention. Each renewal
// is scheduled from the latest deadline Racer has granted and from the time
// the previous response arrived, see nextWarm. A refused (zero) or already
// expired grant, or an error, does not replace a previous deadline that is
// still in the future, since Racer keeps an existing hold until it expires.
func (b *Backend) refreshWarm(retainUntil time.Time) {
	t := time.NewTimer(nextWarm(time.Now(), retainUntil, b.cfg.WarmTTL))
	defer t.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}

		res, err := b.src.Warm(b.ctx, b.cfg.WarmTTL, b.etag)
		if b.ctx.Err() != nil {
			return
		}

		now := time.Now()

		switch {
		case err != nil:
			b.logWarmError(err)
		case res.RetainUntil.After(retainUntil):
			retainUntil = res.RetainUntil
		case res.RetainUntil.IsZero() && b.limiter.allow("warm: refused"):
			b.log.Warn("racer refused warm retention", "etag", b.etag)
		}

		if !retainUntil.After(now) {
			retainUntil = time.Time{}
		}

		t.Reset(nextWarm(now, retainUntil, b.cfg.WarmTTL))
	}
}

func (b *Backend) logWarmError(err error) {
	if !b.limiter.allow("warm: " + err.Error()) {
		return
	}

	level := slog.LevelWarn
	if !retryable(err) {
		level = slog.LevelError
	}

	b.log.Log(b.ctx, level, "racer warm refresh failed", "etag", b.etag, "error", err)
}
