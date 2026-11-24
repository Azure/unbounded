// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"net/http"
	"testing"
	"time"
)

// TestNewEdgeClientTransportTuning locks in the connection-pool
// sizing that the bench / scenario / roundtrip subcommands depend on
// for parallel-GET throughput. The stdlib default (MaxIdleConnsPerHost=2)
// would silently cap effective concurrency to 2 against a single
// orca host, which is exactly the kind of regression a future
// refactor of newEdgeClient could reintroduce by accident.
func TestNewEdgeClientTransportTuning(t *testing.T) {
	t.Parallel()

	c := newEdgeClient("http://localhost:8443", time.Second)

	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("edge client Transport = %T; want *http.Transport", c.http.Transport)
	}

	if tr.MaxIdleConnsPerHost != edgeMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d; want %d (raised from stdlib default of 2 so bench concurrency does not thrash TCP connections)",
			tr.MaxIdleConnsPerHost, edgeMaxIdleConnsPerHost)
	}

	if tr.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d; want 0 (unlimited) so we never silently throttle the operator's concurrency choice",
			tr.MaxConnsPerHost)
	}
}

// TestNewEdgeClientTimeout sanity-checks the 5x-timeout cap on the
// whole-request deadline. Keeps a future "let's just remove the
// Timeout field, the context handles it" refactor from silently
// dropping the multi-GiB safety net.
func TestNewEdgeClientTimeout(t *testing.T) {
	t.Parallel()

	c := newEdgeClient("http://localhost:8443", 30*time.Second)

	if want := 150 * time.Second; c.http.Timeout != want {
		t.Errorf("edge client Timeout = %v; want %v (5 * per-op timeout)", c.http.Timeout, want)
	}
}
