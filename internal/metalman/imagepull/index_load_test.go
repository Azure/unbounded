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
	c := newIndexCache(4)
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

func TestLoadLeaderCancelDoesNotFailLiveWaiter(t *testing.T) {
	c := newIndexCache(4)
	started := make(chan struct{})
	want := &layerIndex{}

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderDone := make(chan error, 1)

	go func() {
		_, err := c.load(leaderCtx, testDigest, func(ctx context.Context) (*layerIndex, error) {
			close(started)
			<-ctx.Done()

			return nil, ctx.Err()
		})
		leaderDone <- err
	}()

	<-started

	waiterDone := make(chan error, 1)
	builds := make(chan struct{}, 4)

	go func() {
		idx, err := c.load(t.Context(), testDigest, func(context.Context) (*layerIndex, error) {
			builds <- struct{}{}
			return want, nil
		})
		if err == nil && idx != want {
			err = errors.New("wrong index")
		}

		waiterDone <- err
	}()

	// Let the waiter join the in-flight build before canceling the leader.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want canceled", err)
	}

	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter err = %v", err)
	}

	if len(builds) != 1 {
		t.Fatalf("waiter builds = %d, want 1 retry", len(builds))
	}
}

func TestLoadOwnBuildErrorReturned(t *testing.T) {
	c := newIndexCache(4)
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
