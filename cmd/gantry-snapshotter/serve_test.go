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

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// writeDevices renders a device description file for set into dir.
func writeDevices(t *testing.T, dir string, set *segment.Set) {
	t.Helper()

	desc, err := set.CatalogDevice()
	if err != nil {
		t.Fatalf("catalog device: %v", err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, `{"generation":%d,"catalog":{"device":%q,"bytes":%d},"segments":[`,
		set.Generation, desc.Device, desc.Bytes)

	for i, seg := range set.Segments {
		if i > 0 {
			b.WriteString(",")
		}

		fmt.Fprintf(&b, `{"id":%d,"device":%q,"bytes":%d}`, seg.ID, seg.Device, seg.Bytes)
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

	runCatalogSync(ctx, &Config{CatalogSync: time.Millisecond}, newHolder(t, false, false), log)

	if strings.Contains(buf.String(), "catalog sync failed") {
		t.Errorf("logged a sync failure for a detached catalog: %s", buf.String())
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
	elector, stop, err := newElector(t.Context(), &Config{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newElector: %v", err)
	}

	defer stop()

	if _, ok := elector.(ingest.Immediate); !ok {
		t.Errorf("elector = %T, want ingest.Immediate", elector)
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
