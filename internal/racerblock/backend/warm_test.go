// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// warmStep scripts one Warm response. grant is relative to the time the
// response is returned; zero grant means refused, and grantAt, when set,
// gives an absolute deadline instead.
type warmStep struct {
	delay   time.Duration // simulated request time before responding
	grant   time.Duration
	grantAt time.Time
	refused bool
	err     error
}

// scriptedWarm is a Source whose Warm follows a script under synctest time.
type scriptedWarm struct {
	*fakeSource

	steps []warmStep
	at    []time.Time // time each Warm call started
}

func (s *scriptedWarm) Warm(ctx context.Context, _ time.Duration, _ string) (racersdk.WarmResult, error) {
	s.at = append(s.at, time.Now())

	step := s.steps[min(len(s.at)-1, len(s.steps)-1)]
	if step.delay > 0 {
		select {
		case <-time.After(step.delay):
		case <-ctx.Done():
			return racersdk.WarmResult{}, ctx.Err()
		}
	}

	if step.err != nil {
		return racersdk.WarmResult{}, step.err
	}

	res := racersdk.WarmResult{Metadata: s.md()}

	switch {
	case step.refused:
	case !step.grantAt.IsZero():
		res.RetainUntil = step.grantAt
	default:
		res.RetainUntil = time.Now().Add(step.grant)
	}

	return res, nil
}

// runWarm opens a backend with WarmTTL ttl, lets fake time run for d, closes
// it, and returns the offsets from Open of each Warm call.
func runWarm(t *testing.T, ttl, d time.Duration, steps ...warmStep) []time.Duration {
	t.Helper()

	var out []time.Duration

	synctest.Test(t, func(t *testing.T) {
		src := &scriptedWarm{fakeSource: newFake(MinChunkSize), steps: steps}
		cfg := testConfig()
		cfg.WarmTTL = ttl

		start := time.Now()

		b, err := Open(t.Context(), src, cfg)
		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(d)

		if err := b.Close(); err != nil {
			t.Fatal(err)
		}

		n := len(src.at)

		time.Sleep(10 * ttl)
		synctest.Wait()

		if len(src.at) != n {
			t.Fatalf("warm called after Close: %d -> %d", n, len(src.at))
		}

		for _, at := range src.at {
			out = append(out, at.Sub(start))
		}
	})

	return out
}

func wantCalls(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("warm calls at %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warm calls at %v, want %v", got, want)
		}
	}
}

// Racer caps the TTL: a 4h request granted 1h must renew before the hour is
// up, not at 2h.
func TestWarmRenewsBeforeShortGrant(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 100*time.Minute, warmStep{grant: time.Hour})
	wantCalls(t, got, 0, 30*time.Minute, time.Hour, 90*time.Minute)
}

// Each renewal is scheduled from its own grant.
func TestWarmFollowsChangingGrant(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 5*time.Hour,
		warmStep{grant: time.Hour},     // Open: next at 30m
		warmStep{grant: 4 * time.Hour}, // 30m: next at 2h30m
		warmStep{grant: 2 * time.Hour}, // 2h30m: old 4h30m deadline still longer, next at 3h30m
		warmStep{grant: 4 * time.Hour}, // 3h30m: until 7h30m, next at 5h30m
	)
	wantCalls(t, got, 0, 30*time.Minute, 150*time.Minute, 210*time.Minute)
}

// Scheduling counts from when the response arrived and against the absolute
// deadline, so a slow request does not push the renewal past the grant.
func TestWarmAccountsForRequestTime(t *testing.T) {
	var start time.Time

	synctest.Test(t, func(t *testing.T) {
		start = time.Now()
	})

	// Under synctest every bubble starts at the same fake time.
	deadline := start.Add(time.Hour)
	got := runWarm(t, 4*time.Hour, 50*time.Minute,
		warmStep{delay: 20 * time.Minute, grantAt: deadline},
	)
	// Open returns at 20m with 40m left: renew at 40m, which again takes
	// 20m and still returns the 1h deadline.
	wantCalls(t, got, 0, 40*time.Minute)
}

// A refused grant never makes racer-block forget a valid earlier grant: it
// keeps renewing at half the time left, no faster than once a second, and
// after the grant expires falls back to WarmTTL/2.
func TestWarmRefusedKeepsPreviousGrant(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 210*time.Minute,
		warmStep{grant: time.Hour}, // next at 30m, deadline 1h
		warmStep{refused: true},
	)
	wantCalls(t, got[:4], 0, 30*time.Minute, 45*time.Minute, 52*time.Minute+30*time.Second)
	checkWarmGaps(t, got, time.Hour, 4*time.Hour)
}

// Errors are handled like refusals: the earlier grant still drives the
// schedule.
func TestWarmErrorKeepsPreviousGrant(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 210*time.Minute,
		warmStep{grant: time.Hour},
		warmStep{err: racersdk.ErrUnavailable},
	)
	wantCalls(t, got[:4], 0, 30*time.Minute, 45*time.Minute, 52*time.Minute+30*time.Second)
	checkWarmGaps(t, got, time.Hour, 4*time.Hour)
}

// A grant already in the past, or refused at Open, is not a deadline: renew
// at WarmTTL/2.
func TestWarmExpiredOrRefusedGrantFallsBack(t *testing.T) {
	for name, step := range map[string]warmStep{
		"expired": {grant: -time.Minute},
		"refused": {refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := runWarm(t, 4*time.Hour, 5*time.Hour, step)
			wantCalls(t, got, 0, 2*time.Hour, 4*time.Hour)
		})
	}
}

// Close stops renewals even while a renewal request is in flight.
func TestWarmCloseDuringRenewal(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 45*time.Minute,
		warmStep{grant: time.Hour},
		warmStep{delay: time.Hour, grant: time.Hour},
	)
	wantCalls(t, got, 0, 30*time.Minute)
}

// checkWarmGaps checks that each call made while the deadline is valid is
// followed by one after half the time left (at least minWarmDelay), that the
// number of such calls stays bounded, and that calls after expiry are
// WarmTTL/2 apart.
func checkWarmGaps(t *testing.T, got []time.Duration, deadline, ttl time.Duration) {
	t.Helper()

	var before int

	for i := 1; i < len(got); i++ {
		gap := got[i] - got[i-1]

		if left := deadline - got[i-1]; left > 0 {
			if want := max(left/2, minWarmDelay); gap != want {
				t.Fatalf("calls %v: gap %v with %v left, want %v", got, gap, left, want)
			}

			before++
		} else if gap != ttl/2 {
			t.Fatalf("calls %v: gap %v after expiry, want %v", got, gap, ttl/2)
		}
	}

	if before < 10 || before > 40 {
		t.Fatalf("calls %v: %d renewals before deadline", got, before)
	}

	if last := got[len(got)-1]; last < deadline+ttl/2-time.Second {
		t.Fatalf("calls %v: no fallback renewal after expiry", got)
	}
}

// Racer may cap every grant at one second. Renewals must still come before
// each deadline, every 500 ms, not at expiry.
func TestWarmRenewsRepeatedOneSecondGrant(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 2100*time.Millisecond, warmStep{grant: time.Second})
	wantCalls(t, got, 0, 500*time.Millisecond, time.Second, 1500*time.Millisecond, 2*time.Second)
}

// When latency leaves less than a second of a grant, the renewal still comes
// before the deadline.
func TestWarmRenewsWithUnderOneSecondLeft(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 900*time.Millisecond, // Close at 1.5s
		warmStep{delay: 600 * time.Millisecond, grant: 400 * time.Millisecond}, // Open: returns at 600ms, deadline 1s
		warmStep{grant: 300 * time.Millisecond},                                // 800ms: deadline 1.1s
		warmStep{grant: 300 * time.Millisecond},                                // 950ms: deadline 1.25s
	)
	wantCalls(t, got, 0, 800*time.Millisecond, 950*time.Millisecond, 1100*time.Millisecond,
		1250*time.Millisecond, 1400*time.Millisecond)
}

// A failed retry with a short deadline left is retried before that deadline.
func TestWarmErrorRetriesBeforeShortDeadline(t *testing.T) {
	got := runWarm(t, 4*time.Hour, 900*time.Millisecond,
		warmStep{grant: time.Second},           // deadline 1s, next at 500ms
		warmStep{err: racersdk.ErrUnavailable}, // 500ms: next at 750ms
		warmStep{grant: time.Second},           // 750ms: deadline 1.75s, next at 1.25s
	)
	wantCalls(t, got, 0, 500*time.Millisecond, 750*time.Millisecond)
}
