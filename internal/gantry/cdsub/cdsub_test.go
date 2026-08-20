// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cdsub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

// fakeSource is a controllable ImageSource for tests.
type fakeSource struct {
	mu          sync.Mutex
	listOut     []ImageEvent
	listErr     error
	listCalls   int32
	subscribeFn func(ctx context.Context) (<-chan ImageEvent, error)
}

func (f *fakeSource) List(_ context.Context) ([]ImageEvent, error) {
	atomic.AddInt32(&f.listCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listErr != nil {
		return nil, f.listErr
	}
	// Return a copy.
	out := make([]ImageEvent, len(f.listOut))
	copy(out, f.listOut)

	return out, nil
}

func (f *fakeSource) Subscribe(ctx context.Context) (<-chan ImageEvent, error) {
	return f.subscribeFn(ctx)
}

func mkDigest(s string) digest.Digest {
	sum := sha256.Sum256([]byte(s))
	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

func TestRun_ReconcilesAndAnnouncesEvents(t *testing.T) {
	d1 := mkDigest("d1")
	d2 := mkDigest("d2")
	d3 := mkDigest("d3")

	ch := make(chan ImageEvent, 4)
	ch <- ImageEvent{Kind: EventCreate, Image: "img-3", Digests: []digest.Digest{d3}}

	src := &fakeSource{
		listOut: []ImageEvent{
			{Kind: EventUpdate, Image: "img-1", Digests: []digest.Digest{d1}},
			{Kind: EventUpdate, Image: "img-2", Digests: []digest.Digest{d2}},
		},
		subscribeFn: func(_ context.Context) (<-chan ImageEvent, error) {
			return ch, nil
		},
	}

	dht := fakes.NewDHT()

	var (
		announceCount  int32
		reconcileCount int32
	)

	sub := New(src, dht,
		WithBackoff(50*time.Millisecond, 100*time.Millisecond),
		WithProvideTimeout(time.Second),
		WithMetrics(
			func() { atomic.AddInt32(&announceCount, 1) },
			nil,
			func(n int) { atomic.StoreInt32(&reconcileCount, int32(n)) },
			nil,
		),
	)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() { done <- sub.Run(ctx) }()

	// Wait for reconcile + event to be processed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&announceCount) >= 3 {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	close(ch) // unblock the loop's select.

	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}

	if got := atomic.LoadInt32(&announceCount); got != 3 {
		t.Errorf("announce count = %d, want 3 (2 reconciled + 1 event)", got)
	}

	if got := atomic.LoadInt32(&reconcileCount); got != 2 {
		t.Errorf("reconcile count = %d, want 2", got)
	}
}

func TestRun_BackoffOnSubscribeError(t *testing.T) {
	d := mkDigest("only")
	src := &fakeSource{
		listOut: []ImageEvent{
			{Kind: EventCreate, Image: "img", Digests: []digest.Digest{d}},
		},
	}

	var attempts int32

	failures := 3
	successCh := make(chan ImageEvent, 1)
	src.subscribeFn = func(_ context.Context) (<-chan ImageEvent, error) {
		n := int(atomic.AddInt32(&attempts, 1))
		if n <= failures {
			return nil, errors.New("transient: containerd socket gone")
		}

		return successCh, nil
	}

	dht := fakes.NewDHT()

	var reconnects int32

	sub := New(src, dht,
		WithBackoff(10*time.Millisecond, 100*time.Millisecond),
		WithMetrics(nil, nil, nil, func() { atomic.AddInt32(&reconnects, 1) }),
	)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() { done <- sub.Run(ctx) }()

	// Wait until we've attempted Subscribe at least 4 times (3 fails + 1 success).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&attempts) >= int32(failures+1) {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	close(successCh)

	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}

	if got := atomic.LoadInt32(&attempts); got < int32(failures+1) {
		t.Errorf("attempts = %d, want >= %d", got, failures+1)
	}
	// Each loop iteration (whether it failed at Subscribe or completed
	// normally) bumps the reconnect metric once before Subscribe runs.
	if got := atomic.LoadInt32(&reconnects); got < int32(failures+1) {
		t.Errorf("reconnects = %d, want >= %d", got, failures+1)
	}
}

func TestRun_DeleteEventIsNoOp(t *testing.T) {
	d := mkDigest("doomed")

	ch := make(chan ImageEvent, 1)
	ch <- ImageEvent{Kind: EventDelete, Image: "old", Digests: []digest.Digest{d}}

	src := &fakeSource{
		listOut: nil,
		subscribeFn: func(_ context.Context) (<-chan ImageEvent, error) {
			return ch, nil
		},
	}
	dht := fakes.NewDHT()

	var announceCount int32

	sub := New(src, dht,
		WithBackoff(50*time.Millisecond, 100*time.Millisecond),
		WithMetrics(func() { atomic.AddInt32(&announceCount, 1) }, nil, nil, nil),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_ = sub.Run(ctx) //nolint:errcheck // best-effort

	if got := atomic.LoadInt32(&announceCount); got != 0 {
		t.Errorf("announce count = %d, want 0 (delete is no-op)", got)
	}
}

func TestRun_NotifierReceivesCreateAndDeleteEvents(t *testing.T) {
	dCreate := mkDigest("created")
	dDelete := mkDigest("deleted")

	ch := make(chan ImageEvent, 2)
	ch <- ImageEvent{Kind: EventCreate, Image: "new", Digests: []digest.Digest{dCreate}}

	ch <- ImageEvent{Kind: EventDelete, Image: "old", Digests: []digest.Digest{dDelete}}

	src := &fakeSource{
		listOut: nil,
		subscribeFn: func(_ context.Context) (<-chan ImageEvent, error) {
			return ch, nil
		},
	}

	type event struct {
		d       digest.Digest
		present bool
	}

	var (
		mu     sync.Mutex
		events []event
	)

	sub := New(src, nil,
		WithBackoff(50*time.Millisecond, 100*time.Millisecond),
		WithNotifier(func(_ context.Context, d digest.Digest, present bool) {
			mu.Lock()
			defer mu.Unlock()

			events = append(events, event{d: d, present: present})
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_ = sub.Run(ctx) //nolint:errcheck // best-effort

	mu.Lock()
	defer mu.Unlock()

	if len(events) != 2 {
		t.Fatalf("notifier events = %d, want 2", len(events))
	}

	if events[0] != (event{d: dCreate, present: true}) {
		t.Fatalf("events[0] = %+v, want create present=true", events[0])
	}

	if events[1] != (event{d: dDelete, present: false}) {
		t.Fatalf("events[1] = %+v, want delete present=false", events[1])
	}
}

func TestRun_ChannelCloseTriggersReconnect(t *testing.T) {
	d := mkDigest("once")
	src := &fakeSource{
		listOut: []ImageEvent{
			{Kind: EventCreate, Image: "x", Digests: []digest.Digest{d}},
		},
	}

	var subCalls int32

	src.subscribeFn = func(_ context.Context) (<-chan ImageEvent, error) {
		c := make(chan ImageEvent)

		atomic.AddInt32(&subCalls, 1)
		close(c) // immediate close = "lost connection"

		return c, nil
	}

	dht := fakes.NewDHT()
	sub := New(src, dht,
		WithBackoff(10*time.Millisecond, 50*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_ = sub.Run(ctx) //nolint:errcheck // best-effort

	if got := atomic.LoadInt32(&subCalls); got < 3 || got > 30 {
		t.Errorf("subscribe calls = %d, want 3..30 (channel-close reconnect with backoff)", got)
	}
}

func TestRun_ListErrorIsBackedOff(t *testing.T) {
	listErr := errors.New("containerd unreachable")
	src := &fakeSource{
		listErr: listErr,
		subscribeFn: func(_ context.Context) (<-chan ImageEvent, error) {
			return nil, errors.New("not reached")
		},
	}
	dht := fakes.NewDHT()
	sub := New(src, dht, WithBackoff(20*time.Millisecond, 100*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := sub.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil, want context error")
	}

	if !strings.Contains(err.Error(), "context") {
		t.Errorf("Run err = %v, want context error", err)
	}

	if atomic.LoadInt32(&src.listCalls) < 2 {
		t.Errorf("list calls = %d, want >= 2 (backoff retried)", src.listCalls)
	}
}

func TestJitter(t *testing.T) {
	d := 100 * time.Millisecond
	for i := 0; i < 100; i++ {
		j := jitter(d)
		if j < 75*time.Millisecond || j > 125*time.Millisecond {
			t.Errorf("jitter(%v) = %v, out of [75ms, 125ms]", d, j)
		}
	}

	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
}
