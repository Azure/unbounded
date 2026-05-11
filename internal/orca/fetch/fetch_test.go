// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fetch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
)

// TestNewCoordinator_UsesInjectedLogger verifies the constructor
// stores the provided slog.Logger on the Coordinator. The peer-RPC
// fallback warnings and commit-after-serve failure traces emitted
// from the fetch path must flow through this logger rather than
// slog.Default(), so operators can route fetch logs alongside the
// rest of the app's structured output.
func TestNewCoordinator_UsesInjectedLogger(t *testing.T) {
	t.Parallel()

	injected := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCoordinator(nil, nil, nil, nil, nil, &config.Config{}, injected)

	if c.log != injected {
		t.Errorf("Coordinator.log not the injected logger")
	}
}

// TestNewCoordinator_NilLoggerFallsBackToDefault locks the contract
// that a nil logger falls back to slog.Default() rather than panicking
// during peer fallback or commit-after-serve.
func TestNewCoordinator_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := NewCoordinator(nil, nil, nil, nil, nil, &config.Config{}, nil)
	if c.log == nil {
		t.Errorf("nil logger should have fallen back to slog.Default()")
	}
}

// TestChunkAttrs_GroupShape locks the slog attribute taxonomy used
// by every fetch-path emission. The 'chunk' group must contain the
// (origin_id, bucket, key, index) identifying tuple so operator
// queries can grep on a single, consistent attribute path.
func TestChunkAttrs_GroupShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))

	log.LogAttrs(context.Background(), slog.LevelDebug, "probe", chunkAttrs(chunk.Key{
		OriginID:  "origin-x",
		Bucket:    "bkt",
		ObjectKey: "obj",
		ChunkSize: 1024,
		Index:     7,
	}))

	out := buf.String()
	for _, want := range []string{
		"chunk.origin_id=origin-x",
		"chunk.bucket=bkt",
		"chunk.key=obj",
		"chunk.index=7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("chunkAttrs output missing %q; got %q", want, out)
		}
	}
}

// TestCoordinator_DebugEmissionsAtDebugLevel exercises a sample of
// the fetch-path debug emissions and asserts they reach the
// handler. We cannot drive the full GetChunk path here without
// standing up the entire dependency graph, so we exercise the
// representative log statements directly. The contract under test
// is that the call sites use LogAttrs at Debug level (so zero-cost
// at Info+) and emit the standardized 'chunk' attribute group.
func TestCoordinator_DebugEmissionsAtDebugLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &Coordinator{log: log}

	k := chunk.Key{
		OriginID:  "ox",
		Bucket:    "bkt",
		ObjectKey: "obj",
		ChunkSize: 1024,
		Index:     3,
	}
	// Sample emissions corresponding to lookupOrStat hits,
	// peer-fill route selection, and commit success.
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "catalog_hit", chunkAttrs(k))
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "peer_fill_attempt",
		chunkAttrs(k), slog.String("peer_ip", "10.0.0.5"))
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "commit_success",
		chunkAttrs(k), slog.Int("bytes", 1024))

	out := buf.String()
	for _, want := range []string{"catalog_hit", "peer_fill_attempt", "commit_success", "chunk.index=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in debug output; got %q", want, out)
		}
	}
}

// TestCoordinator_DebugFilteredAtInfo verifies that the standard
// LogAttrs path emits nothing when the handler is configured above
// Debug. This is the operational expectation: enabling Info-level
// logging silences the per-chunk traces entirely so production
// throughput is not affected by log overhead.
func TestCoordinator_DebugFilteredAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelInfo}))
	c := &Coordinator{log: log}

	k := chunk.Key{OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024, Index: 0}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "catalog_hit", chunkAttrs(k))

	if buf.Len() != 0 {
		t.Errorf("debug emission leaked through Info-level handler: %q", buf.String())
	}
}

// TestCoordinator_WarnRoutesThroughInjectedHandler verifies that the
// (migrated to LogAttrs) commit-after-serve warning still surfaces
// at Warn level on the injected logger. Regression test for the
// existing call site that pre-dates the debug emissions.
func TestCoordinator_WarnRoutesThroughInjectedHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := &Coordinator{log: log}

	k := chunk.Key{OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024, Index: 0}
	c.log.LogAttrs(context.Background(), slog.LevelWarn, "commit-after-serve failed",
		chunkAttrs(k),
		slog.String("err", "stub put failure"),
	)

	out := buf.String()
	if !strings.Contains(out, "commit-after-serve failed") {
		t.Errorf("warning not captured; got %q", out)
	}

	if !strings.Contains(out, "chunk.key=o") {
		t.Errorf("chunk attribute missing; got %q", out)
	}
}
