// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestSnapshotBlockedWriteClosesAtPinnedFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		configureFixtureAge(t, f, 5*time.Second)

		server, peer := net.Pipe()
		defer server.Close()
		defer peer.Close()

		request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
		request.TLS = f.requestState(t)
		request = request.WithContext(connectionContext(f.ctx, server))
		w := &pipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server}
		done := make(chan any, 1)

		go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

		synctest.Wait()
		time.Sleep(3 * time.Second)

		if err := f.a.authority.Observe(f.ctx); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Second)

		if aborted := <-done; aborted != http.ErrAbortHandler {
			t.Fatalf("blocked write did not abort: %v", aborted)
		}

		if len(f.a.Server.writes) != 0 {
			t.Fatal("blocked write retained admission")
		}

		if f.a.authority.PublicationReady() != nil {
			t.Fatal("confirmation should allow a new request")
		}
	})
}

type pipeResponse struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *pipeResponse) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func TestRevokedDeltaCannotBorrowNewAuthority(t *testing.T) {
	r := initializedTopology(t)
	p := reconcileTopology(t, r, t.Context())
	copy := *p
	copy.delta = "delta"
	copy.deltaBase = "base"
	delta := copy.ForBase("base")

	writeCtx, cancel, err := copy.writeContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	restore := withdrawPublication(t, r)
	restore()
	reconcileTopology(t, r, t.Context())

	<-writeCtx.Done()

	if _, err := delta.writeTo(writeCtx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("revoked delta: %v", err)
	}

	if _, _, err := copy.writeContext(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("old image borrowed new authority: %v", err)
	}
}

func TestSnapshotAuthorityHeldThroughFlush(t *testing.T) {
	for _, action := range []string{"suspend", "freshness"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				configureFixtureAge(t, f, 5*time.Second)

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true}
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				<-w.entered

				_, err := f.a.authority.Current()
				if err != nil {
					t.Fatal(err)
				}

				if action == "suspend" {
					restore := withdrawPublication(t, f.a.Topology)
					restore()
					reconcileTopology(t, f.a.Topology, f.ctx)
				} else {
					time.Sleep(3 * time.Second)

					if err := f.a.authority.Observe(f.ctx); err != nil {
						t.Fatal(err)
					}

					time.Sleep(2 * time.Second)
				}

				if len(f.a.Server.writes) != 1 || f.a.Server.polls.count() != 1 {
					t.Fatal("flush released admission")
				}

				close(w.unblock)

				if aborted := <-done; aborted != http.ErrAbortHandler {
					t.Fatalf("revoked flush completed: %v", aborted)
				}

				if len(f.a.Server.writes) != 0 || f.a.Server.polls.count() != 0 {
					t.Fatal("aborted flush leaked admission")
				}
			})
		})
	}
}

func (w *pipeResponse) Write(b []byte) (int, error) { return w.conn.Write(b) }
