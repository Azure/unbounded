// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLifecycleProcessContext(t *testing.T) {
	for _, source := range []string{"parent", "process", "child"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				process, stopProcess := context.WithCancel(t.Context())
				defer stopProcess()

				parent, stopParent := context.WithTimeout(t.Context(), time.Minute)
				defer stopParent()

				key := connectionKey{}
				parent = context.WithValue(parent, key, source)
				l := NewLifecycle(nil)
				l.process = process

				child, cancel := l.ProcessContext(parent)
				defer cancel()

				deadline, ok := child.Deadline()

				parentDeadline, _ := parent.Deadline()
				if child.Err() != nil || child.Value(key) != source || !ok || deadline != parentDeadline {
					t.Fatal("child did not retain parent values, deadline, and live context")
				}

				switch source {
				case "parent":
					stopParent()
				case "process":
					stopProcess()
				case "child":
					cancel()
				}

				synctest.Wait()

				if !errors.Is(child.Err(), context.Canceled) {
					t.Fatalf("child ignored %s cancellation: %v", source, child.Err())
				}

				if source != "process" && process.Err() != nil || source != "parent" && parent.Err() != nil {
					t.Fatal("child cancellation propagated to an independent parent")
				}
			})
		})
	}

	process, cancel := context.WithCancel(t.Context())
	cancel()

	for _, l := range []*Lifecycle{nil, NewLifecycle(nil), {process: process}} {
		child, stop := l.ProcessContext(t.Context())
		if !errors.Is(child.Err(), context.Canceled) {
			t.Fatal("absent or canceled process did not immediately cancel child")
		}

		stop()
	}
}

func TestLifecycleHTTPReadinessTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Server, bool)
	}{
		{name: "issuer", set: func(s *Server, ready bool) {
			if ready {
				restoreServerTrust(t, s)
			} else {
				withdrawServerTrust(t, s)
			}
		}},
		{name: "serving", set: func(s *Server, ready bool) { s.Lifecycle.SetServingReady(ready) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServingFixture(t)
			handler := f.a.Server.Handler()

			tc.set(f.a.Server, false)

			for _, step := range []struct {
				name  string
				ready bool
			}{
				{name: "initial false"},
				{name: "become ready", ready: true},
				{name: "remain ready", ready: true},
				{name: "withdraw readiness"},
				{name: "remain unready"},
				{name: "restore readiness", ready: true},
			} {
				t.Run(step.name, func(t *testing.T) {
					tc.set(f.a.Server, step.ready)

					r := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
					r.TLS = f.requestState(t)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)

					want := http.StatusServiceUnavailable
					if step.ready {
						want = http.StatusOK
					}

					responseBody(t, w.Result(), nil, want)
				})
			}
		})
	}
}

func TestLifecycleFollowerWithValidatedPublicationIsReady(t *testing.T) {
	r := initializedTopology(t)
	reconcileTopology(t, r, t.Context())

	if err := r.authority.PublicationReady(); err != nil {
		t.Fatal(err)
	}

	l := NewLifecycle(r.authority)
	l.process, l.synced = t.Context(), true
	l.SetServingReady(true)

	if err := l.Ready(nil); err != nil {
		t.Fatalf("follower with validated state must receive Service traffic: %v", err)
	}
}

func TestLifecycleGatesAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)

		l := NewLifecycle(r.authority)
		if l.NeedLeaderElection() || l.Ready(nil) == nil {
			t.Fatal("follower ready")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		syncCache := make(chan struct{})
		l.waitForCacheSync = func(ctx context.Context) bool {
			select {
			case <-ctx.Done():
				return false
			case <-syncCache:
				return true
			}
		}
		started := make(chan error, 1)

		go func() { started <- l.Start(ctx) }()

		l.SetServingReady(true)
		reconcileTopology(t, r, ctx)

		if l.Ready(nil) == nil {
			t.Fatal("ready before synchronized inputs")
		}

		close(syncCache)
		synctest.Wait()

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		restore := withdrawPublication(t, r)

		if l.Ready(nil) == nil {
			t.Fatal("ready without publication authority")
		}

		restore()
		reconcileTopology(t, r, ctx)
		l.SetServingReady(false)

		if l.Ready(nil) == nil {
			t.Fatal("ready without listener")
		}

		cancel()

		if err := <-started; err != nil {
			t.Fatal(err)
		}

		l.SetServingReady(true)

		if l.Ready(nil) == nil {
			t.Fatal("old process resurrected")
		}

		request, stop := l.ProcessContext(t.Context())
		defer stop()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request resurrected: %v", request.Err())
		}

		if err := l.Start(context.Background()); !errors.Is(err, wire.Conflict) {
			t.Fatalf("process restarted: %v", err)
		}
	})
}

func TestLifecycleRequiresPublicationAndHonorsRequestCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)
		l := NewLifecycle(r.authority)
		l.waitForCacheSync = func(context.Context) bool { return true }

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- l.Start(ctx) }()

		l.SetServingReady(true)
		synctest.Wait()

		requestCtx, stop := context.WithCancel(context.Background())
		stop()

		request, stopRequest := l.ProcessContext(requestCtx)
		defer stopRequest()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request ignores cancellation: %v", request.Err())
		}

		if err := l.Ready(nil); !errors.Is(err, wire.Unavailable) {
			t.Fatalf("ready without publication: %v", err)
		}

		reconcileTopology(t, r, ctx)

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		require.ErrorIs(t, l.Ready(nil), wire.Unavailable)

		beforeStartup := NewLifecycle(r.authority)
		beforeStartup.waitForCacheSync = func(ctx context.Context) bool { return ctx.Err() == nil }
		require.NoError(t, beforeStartup.Start(ctx))
		beforeStartup.SetServingReady(true)
		require.ErrorIs(t, beforeStartup.Ready(nil), wire.Unavailable, "canceled startup became ready")
	})
}

func TestLifecycleHTTPStartupAdmission(t *testing.T) {
	for _, scenario := range []string{"synchronized", "cache failed", "canceled during sync"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				l := NewLifecycle(f.a.authority)
				f.a.Server.Lifecycle = l
				l.SetServingReady(true)

				syncCache := make(chan struct{})
				l.waitForCacheSync = func(ctx context.Context) bool {
					select {
					case <-ctx.Done():
						return false
					case <-syncCache:
						return scenario == "synchronized"
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				done := make(chan error, 1)

				go func() { done <- l.Start(ctx) }()

				synctest.Wait()

				handler := f.a.Server.Handler()
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				before := httptest.NewRecorder()
				handler.ServeHTTP(before, request)
				responseBody(t, before.Result(), nil, http.StatusServiceUnavailable)

				if scenario == "canceled during sync" {
					cancel()
				} else {
					close(syncCache)
				}

				synctest.Wait()

				after := httptest.NewRecorder()
				handler.ServeHTTP(after, request)

				want := http.StatusServiceUnavailable
				if scenario == "synchronized" {
					want = http.StatusOK
				}

				responseBody(t, after.Result(), nil, want)

				cancel()

				err := <-done
				if scenario == "cache failed" {
					if !errors.Is(err, wire.Unavailable) {
						t.Fatalf("cache failure: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}

				stopped := httptest.NewRecorder()
				handler.ServeHTTP(stopped, request)
				responseBody(t, stopped.Result(), nil, http.StatusServiceUnavailable)
			})
		})
	}
}
