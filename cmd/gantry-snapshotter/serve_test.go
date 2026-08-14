// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// writeDevices renders a device description file for set into dir.
func writeDevices(t *testing.T, dir string, set *segment.Set) {
	t.Helper()

	var b strings.Builder

	fmt.Fprintf(&b, `{"generation":%d,"universe":%d,"device":%q,"catalogBytes":%d,"segments":[`,
		set.Generation, set.Universe, set.Device, set.CatalogBytes)

	for i, seg := range set.Segments {
		if i > 0 {
			b.WriteString(",")
		}

		fmt.Fprintf(&b, `{"id":%d,"offset":%d,"bytes":%d,"epoch":%d}`, seg.ID, seg.Offset, seg.Bytes, seg.Epoch)
	}

	b.WriteString("]}")

	if err := os.WriteFile(filepath.Join(dir, "image-devices.json"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write devices: %v", err)
	}
}

// runReconcile has to attach the catalog once the device description appears,
// which on a fresh node is after the daemon is already serving.
func TestRunReconcileAttachesLate(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)
	path := filepath.Join(dir, "image-devices.json")

	watcher := segment.NewWatcher(segment.WatcherOptions{Path: path, Interval: time.Millisecond})

	cat := newHolder(t, true, true)
	defer cat.close() //nolint:errcheck // test cleanup

	cfg := &Config{DeviceInterval: time.Millisecond}
	signal := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		watcher.Run(ctx)
	}()

	go func() {
		defer wg.Done()

		runReconcile(ctx, cfg, watcher, cat, signal, slog.New(slog.DiscardHandler))
	}()

	// Nothing is published yet, so nothing can attach.
	time.Sleep(10 * time.Millisecond)

	if store, _ := cat.current.load(); store != nil {
		t.Fatal("a catalog attached before any device was published")
	}

	writeDevices(t, dir, set)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store, _ := cat.current.load(); store != nil {
			cancel()
			wg.Wait()

			return
		}

		time.Sleep(time.Millisecond)
	}

	cancel()
	wg.Wait()
	t.Fatal("the catalog never attached")
}

// A device description that names a catalog this node cannot open must be
// logged once, not once per tick.
func TestRunReconcileLogsAFailureOnce(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)
	writeDevices(t, dir, set)

	path := filepath.Join(dir, "image-devices.json")

	watcher := segment.NewWatcher(segment.WatcherOptions{Path: path, Interval: time.Millisecond})
	if err := watcher.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Formatting is off and the device is blank, so every attempt fails
	// with the same error.
	cat := newHolder(t, false, true)

	var buf syncBuffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()

	runReconcile(ctx, &Config{DeviceInterval: time.Millisecond}, watcher, cat, make(chan struct{}), log)

	if got := strings.Count(buf.String(), "catalog unavailable"); got != 1 {
		t.Errorf("logged the same failure %d times, want 1", got)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger on another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// A detached catalog is the expected state on a node whose image device has
// not arrived, so the sync loop must not log about it.
func TestRunCatalogSyncIgnoresADetachedCatalog(t *testing.T) {
	var buf syncBuffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	runCatalogSync(ctx, &Config{CatalogSync: time.Millisecond, HoleGrace: time.Hour}, newHolder(t, false, false), log)

	if strings.Contains(buf.String(), "catalog sync failed") {
		t.Errorf("logged a sync failure for a detached catalog: %s", buf.String())
	}

	if strings.Contains(buf.String(), "catalog repair failed") {
		t.Errorf("logged a repair failure for a detached catalog: %s", buf.String())
	}
}

// Repair is off when the grace is zero, so an operator can pin a hole in place
// while they work out what left it there.
func TestRepairHoleDisabled(t *testing.T) {
	var buf syncBuffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	repairHole(&Config{}, newHolder(t, false, false), log)

	if buf.String() != "" {
		t.Errorf("a disabled repair logged: %s", buf.String())
	}
}

func TestRunCatalogSyncDisabled(t *testing.T) {
	// A zero interval disables the loop, so it must return rather than
	// spin or block on a never-firing ticker.
	done := make(chan struct{})

	go func() {
		defer close(done)

		runCatalogSync(t.Context(), &Config{}, newHolder(t, false, false), slog.New(slog.DiscardHandler))
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCatalogSync did not return with the poll disabled")
	}
}

func TestRunCleanupDisabled(t *testing.T) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		runCleanup(t.Context(), &Config{}, nil, slog.New(slog.DiscardHandler))
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCleanup did not return with the sweep disabled")
	}
}

func TestObserver(t *testing.T) {
	var buf syncBuffer

	observe := observer(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	req := ingest.Request{DiffID: catalog.Digest{1}, ChainID: catalog.Digest{2}}

	observe(req, ingest.Result{Outcome: ingest.OutcomeIngested}, nil)

	if !strings.Contains(buf.String(), "ingest complete") {
		t.Errorf("success was not logged: %s", buf.String())
	}

	observe(req, ingest.Result{}, errors.New("boom"))

	if !strings.Contains(buf.String(), "ingest failed") {
		t.Errorf("failure was not logged: %s", buf.String())
	}
}

// Without a peer view every node ingests eagerly, which is what a single node
// wants and the only safe default when there is nothing to rank against.
func TestNewElectorWithoutAPeerView(t *testing.T) {
	elector, peers, stop, err := newElector(t.Context(), &Config{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newElector: %v", err)
	}

	defer stop()

	if _, ok := elector.(ingest.Immediate); !ok {
		t.Errorf("elector = %T, want ingest.Immediate", elector)
	}

	// No peer view either, which is what makes the cleaner run unelected: one
	// node cannot lose a rendezvous to itself.
	if peers != nil {
		t.Error("peers = non-nil, want nil without a membership view")
	}
}

func TestStopServer(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Second} {
		server := grpc.NewServer()

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}

		done := make(chan struct{})

		go func() {
			defer close(done)

			_ = server.Serve(listener) //nolint:errcheck // stopped below
		}()

		stopServer(server, grace)

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("grace %s: the server did not stop", grace)
		}
	}
}

// blockedTask stands in for the background tasks serve starts. Every one of
// them returns only on ctx.Done(), which is the whole reason awaitShutdown has
// to cancel rather than just wait.
func blockedTask(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		<-ctx.Done()
	}()

	return &wg
}

// runAwaitShutdown calls awaitShutdown with a deadline, so a regression that
// reintroduces the wait-without-cancel deadlock fails the test instead of
// wedging the suite.
func runAwaitShutdown(t *testing.T, fn func() error) error {
	t.Helper()

	result := make(chan error, 1)

	go func() { result <- fn() }()

	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("awaitShutdown did not return: the background tasks were never cancelled")

		return nil
	}
}

// A fatal accept error is not a signal, so awaitShutdown has to cancel the
// background tasks itself before waiting on them.
func TestAwaitShutdownCancelsBackgroundTasksOnServeError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wg := blockedTask(ctx)

	serveErr := make(chan error, 1)
	serveErr <- errors.New("accept: boom")

	err := runAwaitShutdown(t, func() error {
		return awaitShutdown(ctx, cancel, grpc.NewServer(), serveErr, time.Second, wg, slog.New(slog.DiscardHandler))
	})
	if err == nil {
		t.Fatal("awaitShutdown() = nil, want the accept error")
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("awaitShutdown() = %v, want it to carry the accept error", err)
	}

	if ctx.Err() == nil {
		t.Error("the context was not cancelled, so the background tasks were left running")
	}
}

// A server that stopped on its own is not a failure worth reporting.
func TestAwaitShutdownIgnoresServerStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wg := blockedTask(ctx)

	serveErr := make(chan error, 1)
	serveErr <- grpc.ErrServerStopped

	if err := runAwaitShutdown(t, func() error {
		return awaitShutdown(ctx, cancel, grpc.NewServer(), serveErr, time.Second, wg, slog.New(slog.DiscardHandler))
	}); err != nil {
		t.Errorf("awaitShutdown() = %v, want nil", err)
	}
}

// The signal path stops the server and drains Serve, so no goroutine outlives
// the call.
func TestAwaitShutdownDrainsServeOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wg := blockedTask(ctx)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	serveErr := make(chan error, 1)
	served := make(chan struct{})

	go func() {
		defer close(served)

		serveErr <- server.Serve(listener)
	}()

	cancel()

	if err := runAwaitShutdown(t, func() error {
		return awaitShutdown(ctx, cancel, server, serveErr, time.Second, wg, slog.New(slog.DiscardHandler))
	}); err != nil {
		t.Errorf("awaitShutdown() = %v, want nil", err)
	}

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Error("Serve was never drained")
	}
}

func TestCatalogConflictErrnos(t *testing.T) {
	errnos, err := catalogConflictErrnos(&Config{})
	if err != nil {
		t.Fatalf("default: %v", err)
	}

	if len(errnos) == 0 {
		t.Error("want the built-in defaults")
	}

	if _, err := catalogConflictErrnos(&Config{ConflictErrnos: "ENOSUCHTHING"}); err == nil {
		t.Error("want an error for an unknown errno")
	}
}

// The refresh cadence is derived from the grace rather than configured, so the
// two cannot be set into a combination where a healthy node expires between
// refreshes.
func TestWatermarkInterval(t *testing.T) {
	for _, tc := range []struct {
		grace time.Duration
		want  time.Duration
	}{
		{grace: catalog.DefaultWatermarkGrace, want: 2 * time.Minute},
		{grace: time.Minute, want: 12 * time.Second},
		{grace: 2 * time.Second, want: time.Second},
		{grace: 0, want: time.Second},
	} {
		if got := watermarkInterval(tc.grace); got != tc.want {
			t.Errorf("watermarkInterval(%s) = %s, want %s", tc.grace, got, tc.want)
		}
	}
}

// The loop republishes what the sweep last proved, so a node stays visible to
// the drain gate between sweeps.
func TestRunWatermarkRepublishes(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	store, _ := h.current.load()

	// What a sweep would have left behind, without the write it would have
	// done, so the loop's own write is the only thing that can be observed.
	h.published.Store(9)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		runWatermark(ctx, &Config{WatermarkGrace: 2 * time.Second}, h, slog.New(slog.DiscardHandler))
	}()

	deadline := time.Now().Add(10 * time.Second)

	for {
		mark, ok := watermarkFor(t, store, h.node)
		if ok && mark.Generation == 9 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the refresh loop never republished the watermark")
		}

		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runWatermark did not return on cancel")
	}
}

// A detached catalog is not a failure to report: there is no gate to be
// visible to, and reconcile is already saying why.
func TestRunWatermarkIgnoresADetachedCatalog(t *testing.T) {
	var buf syncBuffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()

	runWatermark(ctx, &Config{WatermarkGrace: 2 * time.Second}, newHolder(t, false, false), log)

	if buf.String() != "" {
		t.Errorf("a detached catalog logged: %s", buf.String())
	}
}

// The drain gate waits for the nodes this reports, so a node missing from it is
// a node whose mounts can be trimmed away. This node is therefore in the set
// whatever the cluster's view says, because that view lags exactly when a node
// has just started and is already mounting layers.
func TestExpectedNodesAlwaysIncludesSelf(t *testing.T) {
	t.Parallel()

	self := catalog.NodeKeyFor("node-a")

	keys := expectedNodes("node-a", nil)()
	if len(keys) != 1 || keys[0] != self {
		t.Fatalf("expected nodes = %v, want this node alone", keys)
	}

	// A view that has not loaded yet, or that has lost this node, still
	// yields this node.
	keys = expectedNodes("node-a", func() []ifaces.Node { return nil })()
	if len(keys) != 1 || keys[0] != self {
		t.Fatalf("expected nodes = %v with an empty view, want this node alone", keys)
	}
}

func TestExpectedNodesMergesThePeerView(t *testing.T) {
	t.Parallel()

	peers := func() []ifaces.Node {
		return []ifaces.Node{
			{ID: "node-b"},
			{ID: "node-a"}, // this node, already in the set
			{ID: ""},       // a pod that has not been scheduled
			{ID: "node-c"},
		}
	}

	keys := expectedNodes("node-a", peers)()

	want := []catalog.NodeKey{
		catalog.NodeKeyFor("node-a"),
		catalog.NodeKeyFor("node-b"),
		catalog.NodeKeyFor("node-c"),
	}

	if len(keys) != len(want) {
		t.Fatalf("expected nodes = %v, want %v", keys, want)
	}

	for i, key := range want {
		if keys[i] != key {
			t.Fatalf("expected node %d = %s, want %s", i, keys[i], key)
		}
	}
}
