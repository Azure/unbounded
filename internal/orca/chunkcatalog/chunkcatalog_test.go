// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chunkcatalog

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// TestNew_UsesInjectedLogger locks the contract that the catalog
// stores the caller's logger rather than slog.Default.
func TestNew_UsesInjectedLogger(t *testing.T) {
	t.Parallel()

	injected := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(16, injected)

	if c.log != injected {
		t.Errorf("Catalog.log not the injected logger")
	}
}

// TestNew_NilLoggerFallsBackToDefault verifies the nil-logger
// fallback so misconfigured callers do not panic on the first
// trace emission.
func TestNew_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := New(16, nil)
	if c.log == nil {
		t.Errorf("nil logger should have fallen back to slog.Default()")
	}
}

// TestRecord_Lookup_Forget exercises the basic LRU operations to
// confirm the Catalog behaviour was not regressed by the logger
// field addition.
func TestRecord_Lookup_Forget(t *testing.T) {
	t.Parallel()

	c := New(16, nil)

	k := chunk.Key{OriginID: "o", Bucket: "b", ObjectKey: "key", ChunkSize: 1024}
	if _, ok := c.Lookup(k); ok {
		t.Fatalf("lookup before record returned hit")
	}

	c.Record(k, cachestore.Info{Size: 1024})

	if info, ok := c.Lookup(k); !ok || info.Size != 1024 {
		t.Errorf("lookup after record: ok=%v info=%+v", ok, info)
	}

	c.Forget(k)

	if _, ok := c.Lookup(k); ok {
		t.Errorf("lookup after forget returned hit")
	}
}

// TestDebugEmissions verifies the catalog emits the standardized
// 'chunk' attribute group at debug level on the four operation
// classes (lookup hit, lookup miss, record insert, forget) and that
// the messages route through the injected logger.
func TestDebugEmissions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New(16, log)

	k := chunk.Key{OriginID: "ox", Bucket: "bkt", ObjectKey: "obj", ChunkSize: 1024, Index: 4}

	c.Lookup(k) // miss
	c.Record(k, cachestore.Info{Size: 1024})
	c.Lookup(k) // hit
	c.Forget(k)

	out := buf.String()
	for _, want := range []string{
		"chunkcatalog_lookup_miss",
		"chunkcatalog_record_insert",
		"chunkcatalog_lookup_hit",
		"chunkcatalog_forget",
		"chunk.index=4",
		"chunk.key=obj",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in debug output; got %q", want, out)
		}
	}
}

// TestDebugFilteredAtInfo verifies the catalog emits nothing when
// the handler is configured above Debug, so the hot-path overhead
// at production levels is just the handler's level check.
func TestDebugFilteredAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := New(16, log)

	k := chunk.Key{OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024}
	c.Record(k, cachestore.Info{Size: 1024})
	c.Lookup(k)
	c.Forget(k)

	if buf.Len() != 0 {
		t.Errorf("debug emission leaked through Info-level handler: %q", buf.String())
	}
}

// TestEvictEmitsAttr ensures the LRU-eviction debug emission fires
// when capacity is exceeded. Capacity 1 plus two distinct inserts
// forces an eviction observable via the evicted_path attribute.
func TestEvictEmitsAttr(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New(1, log)

	k1 := chunk.Key{OriginID: "o", Bucket: "b", ObjectKey: "a", ChunkSize: 1024}
	k2 := chunk.Key{OriginID: "o", Bucket: "b", ObjectKey: "b", ChunkSize: 1024}

	c.Record(k1, cachestore.Info{Size: 1024})
	c.Record(k2, cachestore.Info{Size: 1024})

	if !strings.Contains(buf.String(), "chunkcatalog_evict") {
		t.Errorf("evict emission missing from output: %q", buf.String())
	}
}
