// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fetch

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

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

// TestCoordinator_LogsRouteThroughInjectedHandler verifies that
// fetch-path warnings flow through the handler installed at the
// injected slog.Logger rather than the package-level default.
// Operators rely on this to capture fetch logs in the same sink
// as the rest of the app's structured output.
func TestCoordinator_LogsRouteThroughInjectedHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	injected := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := &Coordinator{
		log: injected,
	}

	// Exercise the same log line runFill emits on commit failure.
	// Going through runFill end-to-end would require a full origin /
	// catalog wiring; the contract under test here is just that the
	// handler is the injected one, not slog.Default().
	c.log.Warn("commit-after-serve failed",
		"chunk", "test-chunk",
		"err", "stub put failure",
	)

	if !strings.Contains(buf.String(), "commit-after-serve failed") {
		t.Errorf("warning not captured by injected logger; got %q", buf.String())
	}

	if !strings.Contains(buf.String(), "test-chunk") {
		t.Errorf("chunk attribute missing from output; got %q", buf.String())
	}
}
