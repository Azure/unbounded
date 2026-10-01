// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReplicaObservedHighWaterWithoutImage(t *testing.T) {
	for _, initial := range []string{"installed10", "no image", "suspended"} {
		for _, mutation := range []string{"rollback10", "conflicting11", "membership hash", "membership rollback", "membership jump"} {
			t.Run(initial+"/"+mutation, func(t *testing.T) {
				f := newServingFixture(t)
				r := f.a.Replication
				p := r.Publications
				base := *p.current

				base.record.Sequence, base.record.MembershipVersion = 10, 5
				if err := p.Install(&base); err != nil {
					t.Fatal(err)
				}

				if initial == "no image" {
					p.current = nil
					p.observed = VersionRecord{}
				}

				if initial == "suspended" {
					p.Suspend()
				}

				newer := base.record
				newer.Sequence = 11
				newer.ContentHash = strings.Repeat("a", 64)
				setVersion := func(record VersionRecord) {
					cm, _, err := readVersion(f.ctx, r.APIReader, r.Config)
					if err != nil {
						t.Fatal(err)
					}

					cm.Data = versionData(record)
					if err := r.Client.Update(f.ctx, cm); err != nil {
						t.Fatal(err)
					}
				}
				setVersion(newer)
				r.observe(f.ctx)

				if p.observed != newer {
					t.Fatal("newer authority forgotten without image")
				}

				bad := newer

				switch mutation {
				case "rollback10":
					bad = base.record
				case "conflicting11":
					bad.ContentHash = strings.Repeat("b", 64)
				case "membership hash":
					bad.Sequence++
					bad.MembershipHash = strings.Repeat("b", 64)
				case "membership rollback":
					bad.Sequence++
					bad.MembershipVersion--
				case "membership jump":
					bad.Sequence++
					bad.MembershipVersion += 2
				}

				setVersion(bad)
				r.observe(f.ctx)

				if !p.suspended || p.observed != newer || f.a.Server.Ready(nil) == nil {
					t.Fatal("invalid authority failed to suspend or erased high-water")
				}

				if _, err := r.Trust.pool(); err == nil {
					t.Fatal("invalid authority retained trust")
				}

				if err := p.Install(&base); err == nil {
					t.Fatal("install bypassed observed high-water")
				}

				// Exercise the public serving boundary, not only store readiness:
				// rejected authority must not expose even the previously valid image.
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				response := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request)

				if response.Code != http.StatusServiceUnavailable {
					t.Fatalf("invalid authority still served snapshot: %d", response.Code)
				}

				// Restoring the last valid authority permits a new reconcile/CAS,
				// without forgetting the observed watermark or reusing revoked bytes.
				setVersion(newer)
				runKeys(t, f.a.Keyring)

				recovered := reconcileTopology(t, f.a.Topology, f.ctx)
				if recovered.record.Sequence != newer.Sequence+1 || recovered.record.MembershipVersion != newer.MembershipVersion {
					t.Fatal("recovery reset counters or changed unchanged membership")
				}

				response = httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request.Clone(f.ctx))

				if response.Code != http.StatusOK || response.Body.String() != recovered.encoded {
					t.Fatalf("reconciled authority not served: %d", response.Code)
				}
			})
		}
	}
}

type firstChunkWriter struct {
	calls            int
	entered, unblock chan struct{}
}

func (w *firstChunkWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		close(w.entered)
		<-w.unblock
	}

	return len(b), nil
}

func TestPublicationWriteAuthorityRevocation(t *testing.T) {
	for _, change := range []string{"superseded", "suspend recover", "confirmation after write admission"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := initializedTopology(t)
				image := reconcileTopology(t, r, t.Context())
				copy := *image
				copy.encoded = strings.Repeat("x", 64*1024)
				w := &firstChunkWriter{entered: make(chan struct{}), unblock: make(chan struct{})}
				done := make(chan error, 1)

				writeCtx, stopWrite, err := copy.writeContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stopWrite()

				go func() { _, err := copy.ForBase("").writeTo(writeCtx, w); done <- err }()

				<-w.entered

				switch change {
				case "superseded":
					next := *image
					next.record.Sequence++

					next.record.ContentHash = strings.Repeat("a", 64)
					if err := r.Publications.Install(&next); err != nil {
						t.Fatal(err)
					}
				case "suspend recover":
					r.Publications.Suspend()

					if err := r.Publications.Install(image); err != nil {
						t.Fatal(err)
					}
				case "confirmation after write admission":
					time.Sleep(20 * time.Second)

					if err := r.Publications.confirm(image.record); err != nil {
						t.Fatal(err)
					}

					time.Sleep(11 * time.Second)
				}

				if r.Publications.Ready(nil) != nil {
					t.Fatal("replacement or confirmed image unavailable")
				}

				close(w.unblock)

				if err := <-done; err == nil || w.calls != 1 {
					t.Fatalf("revoked response continued: calls=%d err=%v", w.calls, err)
				}
			})
		})
	}
}

func TestSnapshotBlockedWriteClosesAtPinnedFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		f.a.Server.Publications.maxAge = 5 * time.Second

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

		p, _ := f.a.Server.Publications.Current()
		if err := f.a.Server.Publications.confirm(p.record); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Second)

		if aborted := <-done; aborted != http.ErrAbortHandler {
			t.Fatalf("blocked write did not abort: %v", aborted)
		}

		if len(f.a.Server.writes) != 0 {
			t.Fatal("blocked write retained admission")
		}

		if f.a.Server.Publications.Ready(nil) != nil {
			t.Fatal("confirmation should allow a new request")
		}
	})
}

type pipeResponse struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *pipeResponse) Write(b []byte) (int, error) { return w.conn.Write(b) }
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

	r.Publications.Suspend()

	if err := r.Publications.Install(p); err != nil {
		t.Fatal(err)
	}

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
				f.a.Server.Publications.maxAge = 5 * time.Second
				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true}
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				<-w.entered

				image, err := f.a.Server.Publications.Current()
				if err != nil {
					t.Fatal(err)
				}

				if action == "suspend" {
					f.a.Server.Publications.Suspend()

					if err := f.a.Server.Publications.Install(image); err != nil {
						t.Fatal(err)
					}
				} else {
					time.Sleep(3 * time.Second)

					if err := f.a.Server.Publications.confirm(image.record); err != nil {
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
