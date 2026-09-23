// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

type pausedForwardAPI struct {
	client.Client
	entered, release chan struct{}
	once             sync.Once
}

func (c *pausedForwardAPI) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if strings.Contains(key.Name, "-f-") {
		c.once.Do(func() {
			close(c.entered)

			select {
			case <-c.release:
			case <-ctx.Done():
			}
		})
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

// Build real production-sized payloads one at a time, including 140 historical
// recipients and three bound records. Avoid warming 1,500 large payloads merely
// to exercise one historical request and one independent current heartbeat.
func newForwardLivenessFixture(t *testing.T) (*Server, *rollout, []*http.Request) {
	t.Helper()

	g := testGeneration(defaultSlots, 1500)
	g.Revision = 31

	old, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	store := stateStore{client: fakeKube(), namespace: "state"}
	next := *g

	next.Revision = 32
	if err := store.commit(t.Context(), &next, nil); err != nil {
		t.Fatal(err)
	}

	var (
		ds       []forwardDecision
		requests []*http.Request
	)

	total := 0

	for i := range 140 {
		m := g.Nodes[fmt.Sprintf("node-%06d", i)]

		data, err := marshalSnapshot(old.snapshot(m.ID))
		if err != nil {
			t.Fatal(err)
		}

		d := forwardDecision{Snapshot: data, PodUID: m.PodUID}
		if err := store.putForwardSnapshot(t.Context(), stateName("default")+"-rollout", d); err != nil {
			t.Fatal(err)
		}

		d.Ref, d.Snapshot = d.snapshotRef(), nil
		ds = append(ds, d)
		total += len(data)
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(t.Context())
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", m.ID)
		controlTLS(req, m.PodUID)
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Boot", fmt.Sprintf("%064x", i+1))
		req.Header.Set("X-Racer-Digest", d.Ref.Digest)
		req.Header.Set("X-Racer-Phase", "2")

		requests = append(requests, req)
		if i < 3 {
			d.Boot = req.Header.Get("X-Racer-Boot")
			ds = append(ds, d)
		}
	}

	index, err := indexGeneration(&next)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	r, err := s.rolloutFor(t.Context(), index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.saveForwards(t.Context(), r, ds); err != nil {
		t.Fatal(err)
	}
	// Current payloads are real, and warm only for the two requests under test.
	for _, req := range requests[:2] {
		node := identityBytes("node", index.byID[req.PathValue("node")])

		e, err := s.current(recipient{identityBytes("universe", "default"), node})
		if err != nil || e == nil {
			t.Fatal("current snapshot", err)
		}
	}

	key := identityBytes("node", "node-000001")
	digest := index.snapshotDigests[key]
	requests[1].Header.Set("X-Racer-Digest", fmt.Sprintf("%x", digest))
	requests[1].Header.Set("X-Racer-Phase", "1")
	t.Logf("slots=%d members=%d historical pods=140 records=%d payload bytes=%d ledger bytes=%d", defaultSlots, len(g.Nodes), len(ds), total, len(r.pointer.Data["forwards"]))

	return s, r, requests
}

func TestForwardReadProductionGeometryDoesNotBlockHeartbeat(t *testing.T) {
	s, r, requests := newForwardLivenessFixture(t)
	api := &pausedForwardAPI{Client: s.controlStore.client, entered: make(chan struct{}), release: make(chan struct{})}
	s.controlStore.client = api

	var once sync.Once

	unblock := func() { once.Do(func() { close(api.release) }) }
	defer unblock()

	start := func(req *http.Request) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)

		go func() {
			w := httptest.NewRecorder()
			s.control(w, req)

			done <- w
		}()

		return done
	}
	forward := start(requests[0])

	select {
	case <-api.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("forward read not reached")
	}

	now := time.Now()
	heartbeat := start(requests[1])
	blocked := false

	select {
	case w := <-heartbeat:
		if w.Code != http.StatusOK {
			t.Errorf("heartbeat: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		blocked = true
	}

	t.Logf("independent heartbeat wait=%s blocked=%t", time.Since(now), blocked)
	unblock()

	w := <-forward

	if blocked {
		<-heartbeat
		t.Error("historical payload read held Server.mu past the control response deadline")
	}

	var command pb.ControlCommand
	if w.Code != http.StatusOK || proto.Unmarshal(w.Body.Bytes(), &command) != nil || command.Revision != 31 || command.Phase != 4 || command.Configuration != nil {
		t.Fatalf("historical replay: HTTP=%d revision=%d phase=%d body=%s", w.Code, command.Revision, command.Phase, w.Body.String())
	}

	if r.acks[requests[1].PathValue("node")].phase != 1 {
		t.Fatal("independent prepare acknowledgment lost")
	}

	requests[0].Header.Set("X-Racer-Needs-Config", "1")

	w = <-start(requests[0])
	if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &command) != nil || command.Configuration == nil {
		t.Fatal("historical resend failed")
	}

	hash := sha256.Sum256(configurationSnapshot(t, command.Configuration))
	if fmt.Sprintf("%x", hash) != requests[0].Header.Get("X-Racer-Digest") {
		t.Fatal("historical resend changed bytes")
	}
}

func TestForwardReadRevalidatesBeforeAuthority(t *testing.T) {
	for _, change := range []string{"revision", "replacement", "unpublished", "unavailable", "ledger", "invalid", "boot", "canceled", "read-error"} {
		t.Run(change, func(t *testing.T) {
			f, r, digest := forwardFixture(t)
			before := r.pointer.Data["forwards"]
			req := snapshotRequest(t, f)
			req.Header.Set("X-Racer-Digest", digest)
			req.Header.Set("X-Racer-Forward-Eligible", digest)
			req.Header.Set("X-Racer-Phase", "0")

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			api := &pausedForwardAPI{Client: f.s.controlStore.client, entered: make(chan struct{}), release: make(chan struct{})}
			f.s.controlStore.client = api

			var once sync.Once

			unblock := func() { once.Do(func() { close(api.release) }) }
			defer unblock()

			done := make(chan *httptest.ResponseRecorder, 1)

			go func() {
				w := httptest.NewRecorder()
				f.s.control(w, req.WithContext(ctx))

				done <- w
			}()

			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("forward read not reached")
			}

			progress := make(chan struct{})

			go func() {
				defer close(progress)

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
						m.PodUID = "replacement"
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
				case "ledger":
					ds, err := r.forwardHistory("default")
					if err != nil {
						t.Error(err)
						return
					}

					d := ds[0]

					d.Boot = strings.Repeat("cd", 32)
					if err := f.s.saveForwards(t.Context(), r, append(ds, d)); err != nil {
						t.Error(err)
					}
				case "invalid":
					r.invalidate()
				case "boot":
					r.acks[f.node] = rolloutAck{boot: strings.Repeat("cd", 32), seen: time.Now()}
				case "canceled":
					cancel()
				case "read-error":
					ds, err := r.forwardHistory("default")
					if err != nil {
						t.Error(err)
						return
					}

					part := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: forwardChunkName(stateName("default"), ds[0].Ref, 0)}}
					if err := api.Delete(t.Context(), part); err != nil {
						t.Error(err)
					}
				}
			}()

			select {
			case <-progress:
			case <-time.After(5 * time.Second):
				t.Fatal("forward read blocked publication")
			}

			unblock()

			w := <-done

			want := map[string]int{"revision": 200, "replacement": 403, "unpublished": 404, "unavailable": 503, "ledger": 200, "invalid": 200, "boot": 409, "canceled": 200, "read-error": 503}[change]
			if w.Code != want {
				t.Fatalf("HTTP=%d want=%d body=%s", w.Code, want, w.Body.String())
			}

			if change == "canceled" && w.Body.Len() != 0 {
				t.Fatal("canceled read delivered authority")
			}

			if change == "revision" || change == "ledger" || change == "invalid" {
				var command pb.ControlCommand
				if proto.Unmarshal(w.Body.Bytes(), &command) != nil || fmt.Sprintf("%x", command.ForwardDigest) != digest {
					t.Fatal("retry lost conditional grant")
				}

				ds, err := f.s.rollouts["default"].forwardHistory("default")
				if err != nil {
					t.Fatal(err)
				}

				if change == "ledger" && len(ds) != 3 {
					t.Fatal("retry overwrote concurrent boot binding")
				}

				for _, d := range ds {
					if d.Boot == req.Header.Get("X-Racer-Boot") && d.Grant != command.Revision {
						t.Fatal("grant used obsolete revision")
					}
				}
			} else if f.durable(t).Data["forwards"] != before {
				t.Fatal("rejected read changed durable ledger")
			}
		})
	}
}

func TestForwardReadWaiterCancelsWithoutAuthority(t *testing.T) {
	f, r, digest := forwardFixture(t)
	before := r.pointer.Data["forwards"]
	api := &pausedForwardAPI{Client: f.s.controlStore.client, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.controlStore.client = api

	var once sync.Once

	unblock := func() { once.Do(func() { close(api.release) }) }
	defer unblock()

	start := func(ctx context.Context) <-chan *httptest.ResponseRecorder {
		req := snapshotRequest(t, f).WithContext(ctx)
		req.Header.Set("X-Racer-Digest", digest)
		req.Header.Set("X-Racer-Forward-Eligible", digest)

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
	case <-api.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("forward reader not reached")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wait := &snapshotWaitContext{Context: ctx, entered: make(chan struct{})}

	second := start(wait)
	select {
	case <-wait.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("forward read gate not reached")
	}

	cancel()

	select {
	case w := <-second:
		if w.Body.Len() != 0 {
			t.Fatal("canceled waiter delivered authority")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiter waited for the active read")
	}

	f.s.mu.Lock()
	changed := r.pointer.Data["forwards"] != before || len(r.acks) != 0
	f.s.mu.Unlock()

	if changed {
		t.Fatal("canceled waiter changed ledger or acknowledgments")
	}

	unblock()

	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("active read: %d %s", w.Code, w.Body.String())
	}
}
