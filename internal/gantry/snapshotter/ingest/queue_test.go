// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func layerDigest(n int) digest.Digest { return digest.MustParse(fmt.Sprintf("sha256:%064x", n)) }

func TestImmediateAndFixed(t *testing.T) {
	d := layerDigest(1)

	if got := (Immediate{}).Delay(d); got != 0 {
		t.Fatalf("Immediate = %s, want 0", got)
	}

	if got := Fixed(3 * time.Second).Delay(d); got != 3*time.Second {
		t.Fatalf("Fixed = %s, want 3s", got)
	}
}

func TestNewHRWValidates(t *testing.T) {
	members := func() []ifaces.Node { return nil }

	if _, err := NewHRW(HRWOptions{Members: members}); !errors.Is(err, errNoSelf) {
		t.Fatalf("err = %v, want errNoSelf", err)
	}

	if _, err := NewHRW(HRWOptions{Self: "a"}); !errors.Is(err, errNoMembers) {
		t.Fatalf("err = %v, want errNoMembers", err)
	}

	if _, err := NewHRW(HRWOptions{Self: "a", Members: members, Jitter: 2}); err == nil {
		t.Fatal("want an error for a jitter above 1")
	}
}

// TestHRWElectsExactlyOneEagerNode is the property the whole scheme rests on:
// for any layer, exactly one node in the cluster starts ingesting at once.
func TestHRWElectsExactlyOneEagerNode(t *testing.T) {
	nodes := []ifaces.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	members := func() []ifaces.Node { return nodes }

	for i := range 64 {
		layer := layerDigest(i)

		eager := 0
		seen := map[time.Duration]int{}

		for _, n := range nodes {
			e, err := NewHRW(HRWOptions{Self: n.ID, Members: members, Jitter: 0.0001})
			if err != nil {
				t.Fatalf("new: %v", err)
			}

			d := e.Delay(layer)
			if d == 0 {
				eager++
			}

			seen[d.Truncate(DefaultStep)]++
		}

		if eager != 1 {
			t.Fatalf("layer %d had %d eager nodes, want 1", i, eager)
		}

		if len(seen) != len(nodes) {
			t.Fatalf("layer %d produced %d distinct ranks, want %d", i, len(seen), len(nodes))
		}
	}
}

func TestHRWDelayGrowsWithRank(t *testing.T) {
	nodes := make([]ifaces.Node, 8)
	for i := range nodes {
		nodes[i] = ifaces.Node{ID: ifaces.NodeID(fmt.Sprintf("n%d", i))}
	}

	layer := layerDigest(7)

	var delays []time.Duration

	for _, n := range nodes {
		e, err := NewHRW(HRWOptions{Self: n.ID, Members: func() []ifaces.Node { return nodes }, Jitter: 0.0001})
		if err != nil {
			t.Fatalf("new: %v", err)
		}

		delays = append(delays, e.Delay(layer))
	}

	for _, d := range delays {
		if d > DefaultMaxDelay {
			t.Fatalf("delay %s exceeds the cap", d)
		}
	}
}

func TestHRWCapsTheDelay(t *testing.T) {
	nodes := make([]ifaces.Node, 200)
	for i := range nodes {
		nodes[i] = ifaces.Node{ID: ifaces.NodeID(fmt.Sprintf("n%d", i))}
	}

	worst := time.Duration(0)

	for _, n := range nodes {
		e, err := NewHRW(HRWOptions{Self: n.ID, Members: func() []ifaces.Node { return nodes }})
		if err != nil {
			t.Fatalf("new: %v", err)
		}

		if d := e.Delay(layerDigest(3)); d > worst {
			worst = d
		}
	}

	if worst != DefaultMaxDelay {
		t.Fatalf("worst delay = %s, want the cap %s", worst, DefaultMaxDelay)
	}
}

func TestHRWUnknownMembershipIngestsNow(t *testing.T) {
	// An unsynced informer must not stall ingest: a duplicate blob is
	// cheaper than a layer that never lands.
	e, err := NewHRW(HRWOptions{Self: "a", Members: func() []ifaces.Node { return nil }})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if d := e.Delay(layerDigest(1)); d != 0 {
		t.Fatalf("delay = %s, want 0", d)
	}

	if d := e.Delay(digest.Digest{}); d != 0 {
		t.Fatalf("empty digest delay = %s, want 0", d)
	}
}

func TestHRWAbsentNodeWaitsTheCap(t *testing.T) {
	e, err := NewHRW(HRWOptions{
		Self:    "draining",
		Members: func() []ifaces.Node { return []ifaces.Node{{ID: "a"}, {ID: "b"}} },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// It defers to the steady nodes, but not forever: it may be the only
	// node holding the layer bytes.
	if d := e.Delay(layerDigest(1)); d != DefaultMaxDelay {
		t.Fatalf("delay = %s, want the cap %s", d, DefaultMaxDelay)
	}
}

// recordingIngester counts Ingest calls against a real Ingester.
type queueHarness struct {
	q    *Queue
	done chan struct{}

	mu      sync.Mutex
	results []Result
	errs    []error
}

func newQueueHarness(t *testing.T, opts QueueOptions, expect int) *queueHarness {
	t.Helper()

	h := &queueHarness{done: make(chan struct{})}

	remaining := expect
	opts.Observe = func(_ Request, res Result, err error) {
		h.mu.Lock()
		h.results = append(h.results, res)
		h.errs = append(h.errs, err)
		remaining--
		last := remaining == 0
		h.mu.Unlock()

		if last {
			close(h.done)
		}
	}

	q, err := NewQueue(opts)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	h.q = q

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go q.Run(ctx)

	return h
}

func (h *queueHarness) wait(t *testing.T) {
	t.Helper()

	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("queue did not finish")
	}
}

func TestNewQueueValidates(t *testing.T) {
	if _, err := NewQueue(QueueOptions{}); err == nil {
		t.Fatal("want an error without an ingester")
	}
}

func TestQueueIngests(t *testing.T) {
	store := newStore(t, 8)
	builder, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	h := newQueueHarness(t, QueueOptions{Ingester: i}, 1)

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2), Layer: layerDigest(1)}
	if !h.q.Submit(req) {
		t.Fatal("submit was rejected")
	}

	h.wait(t)

	if h.errs[0] != nil {
		t.Fatalf("ingest: %v", h.errs[0])
	}

	if h.results[0].Outcome != OutcomeIngested {
		t.Fatalf("outcome = %s, want ingested", h.results[0].Outcome)
	}

	if *calls != 1 {
		t.Fatalf("builder calls = %d, want 1", *calls)
	}

	if _, ok := store.Resolve(req.ChainID); !ok {
		t.Fatal("chain did not resolve")
	}

	if h.q.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", h.q.Pending())
	}
}

func TestQueueDeduplicates(t *testing.T) {
	store := newStore(t, 8)
	builder, _ := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
		Builder: builder,
	})

	// A long delay keeps the request parked so the duplicate lands while it
	// is still pending.
	q, err := NewQueue(QueueOptions{Ingester: i, Elector: Fixed(time.Hour)})
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	req := Request{DiffID: digestOf(1), ChainID: digestOf(2), Layer: layerDigest(1)}

	if !q.Submit(req) {
		t.Fatal("first submit was rejected")
	}

	if q.Submit(req) {
		t.Fatal("duplicate submit was accepted")
	}

	if q.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", q.Pending())
	}
}

func TestQueueRejectsInvalidAndOverflow(t *testing.T) {
	i := newIngester(t, Options{
		Catalog: newStore(t, 8),
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
	})

	q, err := NewQueue(QueueOptions{Ingester: i, Depth: 2, Elector: Fixed(time.Hour)})
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	if q.Submit(Request{DiffID: digestOf(1)}) {
		t.Fatal("a request with no chain id was accepted")
	}

	for n := range 2 {
		if !q.Submit(Request{DiffID: digestOf(byte(n + 1)), ChainID: digestOf(byte(n + 100))}) {
			t.Fatalf("submit %d was rejected", n)
		}
	}

	if q.Submit(Request{DiffID: digestOf(9), ChainID: digestOf(99)}) {
		t.Fatal("a full queue accepted a request")
	}

	// Dropping must not leak the dedup entry, or the layer could never be
	// submitted again.
	if q.Pending() != 2 {
		t.Fatalf("pending = %d, want 2", q.Pending())
	}
}

func TestQueueRetriesOnce(t *testing.T) {
	store := newStore(t, 8)
	builder, calls := fakeBuilder(t, bytes.Repeat([]byte("e"), 4096))
	opener := &bytesOpener{data: []byte("tar"), failFirst: errors.New("blob is not in the content store yet")}

	i := newIngester(t, Options{
		Catalog: store,
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  opener,
		Builder: builder,
	})

	h := newQueueHarness(t, QueueOptions{Ingester: i, RetryDelay: time.Millisecond}, 1)

	// The second attempt succeeds because the blob has arrived by then.
	if !h.q.Submit(Request{DiffID: digestOf(1), ChainID: digestOf(2), Layer: layerDigest(1)}) {
		t.Fatal("submit was rejected")
	}

	h.wait(t)

	if h.errs[0] != nil {
		t.Fatalf("retry did not succeed: %v", h.errs[0])
	}

	if *calls != 1 {
		t.Fatalf("builder calls = %d, want 1", *calls)
	}
}

func TestQueueReportsAPermanentFailure(t *testing.T) {
	want := errors.New("gone for good")

	i := newIngester(t, Options{
		Catalog: newStore(t, 8),
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{err: want},
	})

	h := newQueueHarness(t, QueueOptions{Ingester: i, RetryDelay: time.Millisecond}, 1)

	if !h.q.Submit(Request{DiffID: digestOf(1), ChainID: digestOf(2)}) {
		t.Fatal("submit was rejected")
	}

	h.wait(t)

	if !errors.Is(h.errs[0], want) {
		t.Fatalf("err = %v, want %v", h.errs[0], want)
	}

	if h.q.Pending() != 0 {
		t.Fatalf("a failed request stayed pending: %d", h.q.Pending())
	}
}

func TestQueueStopsOnCancel(t *testing.T) {
	i := newIngester(t, Options{
		Catalog: newStore(t, 8),
		Locator: fileLocator{path: deviceFile(t, 8)},
		Opener:  &bytesOpener{data: []byte("tar")},
	})

	q, err := NewQueue(QueueOptions{Ingester: i, Elector: Fixed(time.Hour)})
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		q.Run(ctx)
		close(done)
	}()

	q.Submit(Request{DiffID: digestOf(1), ChainID: digestOf(2)})
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(t.Context(), 0) {
		t.Fatal("a zero wait on a live context must complete")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if sleepCtx(ctx, 0) {
		t.Fatal("a zero wait on a dead context must not complete")
	}

	if sleepCtx(ctx, time.Hour) {
		t.Fatal("a cancelled wait must not complete")
	}
}

func TestRequestString(t *testing.T) {
	r := Request{DiffID: digestOf(0xab), ChainID: digestOf(0xcd)}

	want := "layer " + r.DiffID.Short() + " chain " + r.ChainID.Short()
	if got := r.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
