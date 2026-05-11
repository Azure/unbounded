// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chunkcatalog

import (
	"io"
	"log/slog"
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
