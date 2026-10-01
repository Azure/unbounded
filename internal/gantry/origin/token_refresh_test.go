// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenRefreshWaitCancellationAndIsolation(t *testing.T) {
	flight := &tokenRefresh{challenge: "scope-a", done: make(chan struct{}), token: "a"}
	r := &registry{tokenRefresh: flight}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.refreshBearerToken(ctx, "scope-a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	close(flight.done)

	if token, err := r.refreshBearerToken(context.Background(), "scope-a"); err != nil || token != "a" {
		t.Fatal(token, err)
	}

	r.tokenRefresh = nil
	if _, err := r.refreshBearerToken(context.Background(), "invalid"); err == nil {
		t.Fatal("invalid challenge accepted")
	}

	if r.tokenRefresh != nil {
		t.Fatal("failed refresh retained")
	}

	r.setToken("new", time.Hour)
	r.clearToken("old")

	if r.cachedToken() != "new" {
		t.Fatal("late rejection cleared refreshed token")
	}
}

func TestTokenRefreshLeaderCancellationPreservesLiveWaiter(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	var releaseOnce sync.Once

	var calls atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}

		select {
		case <-release:
			_, _ = fmt.Fprint(w, `{"token":"anonymous-token","expires_in":300}`)
		case <-req.Context().Done():
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })

	r := &registry{hc: server.Client()}
	challenge := `Bearer realm="` + server.URL + `"`

	leaderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leader := make(chan error, 1)

	go func() {
		_, err := r.refreshBearerToken(leaderCtx, challenge)
		leader <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}

	cancel()

	select {
	case err := <-leader:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not cancel")
	}

	// A live caller can join even after the original caller has departed.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	waiterCtx := &tokenWaitContext{Context: ctx, waiting: make(chan struct{})}

	go func() {
		select {
		case <-waiterCtx.waiting:
			releaseOnce.Do(func() { close(release) })
		case <-ctx.Done():
		}
	}()

	token, err := r.refreshBearerToken(waiterCtx, challenge)
	if err != nil || token != "anonymous-token" {
		t.Fatalf("waiter = %q, %v", token, err)
	}

	if calls.Load() != 1 || r.cachedToken() != token {
		t.Fatalf("calls = %d, cached = %q", calls.Load(), r.cachedToken())
	}
}

type tokenWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *tokenWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestTokenRefreshStaleFlightDoesNotReplaceCurrent(t *testing.T) {
	current := &tokenRefresh{done: make(chan struct{})}
	r := &registry{tokenRefresh: current}
	r.setToken("current", time.Hour)

	stale := &tokenRefresh{challenge: "invalid", done: make(chan struct{})}
	r.runTokenRefresh(stale)

	if r.tokenRefresh != current || r.cachedToken() != "current" {
		t.Fatal("stale flight replaced current state")
	}

	select {
	case <-stale.done:
	default:
		t.Fatal("stale flight waiters not released")
	}
}
