// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

const testDigest = digest.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

func TestLoadWaiterHonorsContext(t *testing.T) {
	c := newIndexCache(4, 0)
	started, release := make(chan struct{}), make(chan struct{})
	want := &layerIndex{}

	leaderDone := make(chan error, 1)

	go func() {
		_, err := c.load(t.Context(), testDigest, func(context.Context) (*layerIndex, error) {
			close(started)
			<-release

			return want, nil
		})
		leaderDone <- err
	}()

	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	begin := time.Now()

	_, err := c.load(ctx, testDigest, func(context.Context) (*layerIndex, error) {
		t.Error("waiter must not build")
		return nil, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want deadline exceeded", err)
	}

	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("waiter blocked %v", d)
	}

	// A live waiter joining later still gets the shared result.
	liveDone := make(chan *layerIndex, 1)

	go func() {
		idx, _ := c.load(t.Context(), testDigest, func(context.Context) (*layerIndex, error) {
			return nil, errors.New("unexpected build")
		})
		liveDone <- idx
	}()

	close(release)

	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err = %v", err)
	}

	if got := <-liveDone; got != want {
		t.Fatalf("live waiter got %p, want %p", got, want)
	}

	if idx, ok := c.get(testDigest); !ok || idx != want {
		t.Fatal("index not cached after build")
	}
}

// The build is detached from the leader: canceling the leader does not
// cancel the build, and a later caller gets its result without building
// again.
func TestLoadLeaderCancelDoesNotCancelBuild(t *testing.T) {
	c := newIndexCache(4, 0)
	started, release := make(chan struct{}), make(chan struct{})
	want := &layerIndex{}
	builds := 0

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderDone := make(chan error, 1)

	go func() {
		_, err := c.load(leaderCtx, testDigest, func(ctx context.Context) (*layerIndex, error) {
			builds++

			close(started)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return want, nil
			}
		})
		leaderDone <- err
	}()

	<-started
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want canceled", err)
	}

	close(release)

	idx, err := c.load(t.Context(), testDigest, func(context.Context) (*layerIndex, error) {
		t.Error("later caller must not build")
		return nil, nil
	})
	if err != nil || idx != want {
		t.Fatalf("later caller = %p, %v; want %p", idx, err, want)
	}

	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
}

func TestLoadBuildTimeoutBoundsDetachedBuild(t *testing.T) {
	c := newIndexCache(4, 50*time.Millisecond)

	var hasDeadline bool

	_, err := c.load(t.Context(), testDigest, func(ctx context.Context) (*layerIndex, error) {
		_, hasDeadline = ctx.Deadline()
		<-ctx.Done()

		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || !hasDeadline {
		t.Fatalf("err = %v hasDeadline = %v, want build deadline exceeded", err, hasDeadline)
	}

	if _, ok := c.get(testDigest); ok {
		t.Fatal("failed build cached")
	}
}

func TestLoadNoBuildTimeoutHasNoDeadline(t *testing.T) {
	c := newIndexCache(4, 0)

	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()

	_, err := c.load(ctx, testDigest, func(ctx context.Context) (*layerIndex, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("detached build inherited the caller deadline")
		}

		return &layerIndex{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadOwnBuildErrorReturned(t *testing.T) {
	c := newIndexCache(4, 0)
	calls := 0

	_, err := c.load(t.Context(), testDigest, func(context.Context) (*layerIndex, error) {
		calls++
		return nil, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("err = %v calls = %d, want own error without retry", err, calls)
	}

	if _, ok := c.get(testDigest); ok {
		t.Fatal("failed build cached")
	}
}

func TestMirrorBudgetUsesLiveWaiterDeadline(t *testing.T) {
	waiter, cancel := context.WithTimeout(t.Context(), 10*time.Second)

	build, cancelBuild := context.WithTimeout(withWaiter(context.WithoutCancel(waiter), waiter), time.Hour)
	defer cancelBuild()

	if limit, ok := mirrorBudget(build); !ok || limit > 5*time.Second {
		t.Fatalf("budget with live waiter = %v, %v; want <= 5s", limit, ok)
	}

	cancel()

	if limit, ok := mirrorBudget(build); !ok || limit < 20*time.Minute {
		t.Fatalf("budget after waiter left = %v, %v; want build bound", limit, ok)
	}

	if _, ok := mirrorBudget(withWaiter(context.Background(), context.Background())); ok {
		t.Fatal("budget without any deadline should be unbounded")
	}
}
