// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
)

func snapshotRequest(t *testing.T, f *coordinationFixture) *http.Request {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil).WithContext(t.Context())
	req.SetPathValue("universe", identity("universe", "default"))
	req.SetPathValue("node", f.node)
	controlTLS(req, "pod-uid")
	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
	req.Header.Set("X-Racer-Profile", "1")
	req.Header.Set("X-Racer-Digest", f.digest)
	req.Header.Set("X-Racer-Phase", "1")

	return req
}

func evictSnapshots(s *Server) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.source.lru.Len() != 0 {
		s.source.remove(s.source.lru.Back())
	}
}

func TestSnapshotBuildDoesNotBlockHeartbeatAndRevalidates(t *testing.T) {
	for _, change := range []string{"heartbeat", "revision", "replacement", "unpublished", "unavailable", "canceled", "build-error"} {
		t.Run(change, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			evictSnapshots(f.s)
			before := f.durable(t).ResourceVersion
			ack := f.s.rollouts["default"].acks[f.node]
			entered, release := make(chan struct{}), make(chan struct{})

			var once sync.Once

			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			var builds atomic.Int64

			f.s.buildSnapshot = func(index *topologyIndex, key recipient) (*entry, error) {
				if builds.Add(1) == 1 {
					close(entered)
					<-release
				}

				if change == "build-error" {
					return nil, errors.New("injected snapshot failure")
				}

				return buildSnapshotEntry(index, key)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			req := snapshotRequest(t, f).WithContext(ctx)
			req.Header.Set("X-Racer-Needs-Config", "1")

			done := make(chan *httptest.ResponseRecorder, 1)

			go func() {
				w := httptest.NewRecorder()
				f.s.control(w, req)

				done <- w
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot builder not reached")
			}

			progress := make(chan struct{})

			go func() {
				defer close(progress)

				if change == "heartbeat" {
					w := httptest.NewRecorder()
					f.s.control(w, snapshotRequest(t, f))

					var c pb.ControlCommand
					if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &c) != nil || c.Phase != 2 || c.Configuration != nil {
						t.Error("cold construction blocked config-free prepare progress")
					}

					return
				}

				f.s.mu.Lock()
				defer f.s.mu.Unlock()

				switch change {
				case "revision", "replacement":
					g := *f.index.g
					g.Revision++
					g.Nodes = maps.Clone(g.Nodes)

					if change == "replacement" {
						name := f.index.byID[f.node]
						m := g.Nodes[name]
						m.PodUID = "replacement-pod"
						g.Nodes[name] = m
					}

					_, pointer, err := f.s.controlStore.load(t.Context(), "default")
					if err != nil {
						t.Error(err)
						return
					}

					if err := f.s.controlStore.commit(t.Context(), &g, pointer); err != nil {
						t.Error(err)
						return
					}

					index, err := indexGeneration(&g)
					if err != nil {
						t.Error(err)
						return
					}

					if err := f.s.installLocked(index); err != nil {
						t.Error(err)
					}
				case "unpublished":
					delete(f.s.source.topologies, identityBytes("universe", "default"))
				case "unavailable":
					f.s.source = nil
				case "canceled":
					cancel()
				}
			}()

			select {
			case <-progress:
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot construction held Server.mu")
			}

			unblock()

			w := <-done

			want := map[string]int{"heartbeat": 200, "revision": 200, "replacement": 403, "unpublished": 404, "unavailable": 503, "canceled": 200, "build-error": 503}[change]
			if w.Code != want {
				t.Fatalf("HTTP=%d want=%d: %s", w.Code, want, w.Body.String())
			}

			if change == "heartbeat" || change == "revision" {
				var c pb.ControlCommand
				if err := proto.Unmarshal(w.Body.Bytes(), &c); err != nil {
					t.Fatal(err)
				}

				raw := configurationSnapshot(t, c.Configuration)

				digest := sha256.Sum256(raw)
				if hex.EncodeToString(digest[:]) != hex.EncodeToString(c.SnapshotDigest) || c.Revision != c.Configuration.GetSnapshot().Revision {
					t.Fatal("command and payload publication disagree")
				}

				if change == "revision" && (c.Revision != 2 || builds.Load() != 2 || len(f.index.snapshotDigests) != 1) {
					t.Fatal("obsolete build was accepted instead of rebuilding current publication")
				}
			} else {
				if f.durable(t).ResourceVersion != before || f.s.rollouts["default"].acks[f.node] != ack {
					t.Fatal("rejected build refreshed acknowledgment or durable rollout")
				}

				if f.s.source != nil && len(f.s.source.cache) != 0 {
					t.Fatal("obsolete, canceled, or failed build polluted payload cache")
				}

				if change == "canceled" && w.Body.Len() != 0 {
					t.Fatal("canceled construction produced a command")
				}
			}
		})
	}
}

func TestSnapshotDigestSurvivesEvictionAndRequiresExactMatch(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.call(t, 0, 200)

	for _, mode := range []string{"match", "wrong", "missing", "resend", "restart"} {
		t.Run(mode, func(t *testing.T) {
			evictSnapshots(f.s)
			req := snapshotRequest(t, f)

			switch mode {
			case "wrong":
				req.Header.Set("X-Racer-Digest", strings.Repeat("ff", 32))
			case "missing":
				req.Header.Del("X-Racer-Digest")
			case "resend":
				req.Header.Set("X-Racer-Needs-Config", "1")
			case "restart":
				index, err := indexGeneration(f.index.g)
				if err != nil {
					t.Fatal(err)
				}

				f.s = &Server{controlStore: f.s.controlStore}
				if err := f.s.install(index); err != nil {
					t.Fatal(err)
				}
			}

			var builds int

			f.s.buildSnapshot = func(index *topologyIndex, key recipient) (*entry, error) {
				builds++
				return buildSnapshotEntry(index, key)
			}
			w := httptest.NewRecorder()
			f.s.control(w, req)

			var c pb.ControlCommand
			if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &c) != nil || hex.EncodeToString(c.SnapshotDigest) != f.digest {
				t.Fatalf("invalid control response: HTTP=%d", w.Code)
			}

			wantBuilds := 1
			if mode == "match" {
				wantBuilds = 0
			}

			wantConfig := mode == "wrong" || mode == "missing" || mode == "resend"
			if builds != wantBuilds || (c.Configuration != nil) != wantConfig {
				t.Fatalf("builds=%d config=%t", builds, c.Configuration != nil)
			}

			if wantConfig {
				digest := sha256.Sum256(configurationSnapshot(t, c.Configuration))
				if hex.EncodeToString(digest[:]) != f.digest {
					t.Fatal("resend changed snapshot digest")
				}
			}
		})
	}
}

// Observe entry into the cold-builder wait without scheduler sleeps.
type snapshotWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *snapshotWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSnapshotColdWaitersCoalesceAndCancel(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.call(t, 0, 200)
	evictSnapshots(f.s)

	entered, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	var builds atomic.Int64

	f.s.buildSnapshot = func(index *topologyIndex, key recipient) (*entry, error) {
		if builds.Add(1) == 1 {
			close(entered)
			<-release
		}

		return buildSnapshotEntry(index, key)
	}
	start := func(ctx context.Context) <-chan *httptest.ResponseRecorder {
		req := snapshotRequest(t, f).WithContext(ctx)
		req.Header.Set("X-Racer-Needs-Config", "1")

		done := make(chan *httptest.ResponseRecorder, 1)

		go func() {
			w := httptest.NewRecorder()
			f.s.control(w, req)

			done <- w
		}()

		return done
	}
	first := start(t.Context())

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first builder not reached")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	waiter := &snapshotWaitContext{Context: ctx, entered: make(chan struct{})}

	canceled := start(waiter)
	select {
	case <-waiter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not reach builder gate")
	}

	cancel()

	select {
	case w := <-canceled:
		if w.Body.Len() != 0 || builds.Load() != 1 {
			t.Fatal("canceled waiter performed snapshot work")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiter waited for the blocked builder")
	}

	waiter = &snapshotWaitContext{Context: t.Context(), entered: make(chan struct{})}

	second := start(waiter)
	select {
	case <-waiter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second request did not reach builder gate")
	}

	unblock()

	for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
		w := <-done

		var c pb.ControlCommand
		if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &c) != nil || c.Configuration == nil {
			t.Fatal("coalesced request lost configuration")
		}
	}

	if builds.Load() != 1 {
		t.Fatal("queued duplicate rebuilt the same snapshot")
	}
}
