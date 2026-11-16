// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newTestHandler builds a pretty handler whose output is captured in the
// returned buffer instead of stderr.
func newTestHandler(t *testing.T, lvl slog.Leveler, opts PrettyFieldHandlerOptions) (slog.Handler, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer

	h, ok := NewPrettyFieldHandler(lvl, opts).(*consoleHandler)
	if !ok {
		t.Fatalf("NewPrettyFieldHandler returned %T, want *consoleHandler", h)
	}

	h.w = &buf

	return h, &buf
}

func newRecord(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Unix(0, 0).UTC(), level, msg, 0)
	r.AddAttrs(attrs...)

	return r
}

func TestPrettyFieldHandlerUsesOptionsLevel(t *testing.T) {
	// A nil positional leveler must fall back to opts.Level, so Debug records
	// are rendered instead of being dropped by the Info default.
	h, buf := newTestHandler(t, nil, PrettyFieldHandlerOptions{Level: slog.LevelDebug})

	if err := h.Handle(context.Background(), newRecord(slog.LevelDebug, "debug message")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "[D]") {
		t.Errorf("output %q missing debug level marker", out)
	}

	if !strings.Contains(out, "debug message") {
		t.Errorf("output %q missing debug message", out)
	}
}

func TestPrettyFieldHandlerFiltersInfoAboveOptionsLevel(t *testing.T) {
	h, buf := newTestHandler(t, nil, PrettyFieldHandlerOptions{Level: slog.LevelWarn})

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(Info) = true with opts.Level Warn, want false")
	}

	if err := h.Handle(context.Background(), newRecord(slog.LevelInfo, "info message")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("output = %q, want empty", buf.String())
	}
}

func TestPrettyFieldHandlerDefaultsToInfo(t *testing.T) {
	h, buf := newTestHandler(t, nil, PrettyFieldHandlerOptions{})

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Enabled(Debug) = true with no level set, want false")
	}

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(Info) = false with no level set, want true")
	}

	if err := h.Handle(context.Background(), newRecord(slog.LevelInfo, "info message")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if !strings.Contains(buf.String(), "info message") {
		t.Errorf("output %q missing info message", buf.String())
	}
}

func TestPrettyFieldHandlerPositionalLevelWins(t *testing.T) {
	// The positional leveler is authoritative when both are set.
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelError)

	h, buf := newTestHandler(t, lvl, PrettyFieldHandlerOptions{Level: slog.LevelDebug})

	if err := h.Handle(context.Background(), newRecord(slog.LevelDebug, "debug message")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("output = %q, want empty", buf.String())
	}
}

func TestPrettyFieldHandlerDeterministicExtraAttrs(t *testing.T) {
	h, buf := newTestHandler(t, nil, PrettyFieldHandlerOptions{
		Level:     slog.LevelDebug,
		AttrOrder: []string{"zulu"},
	})

	rec := newRecord(slog.LevelInfo, "message",
		slog.String("zulu", "ordered"),
		slog.String("alpha", "1"),
		slog.String("mike", "2"),
		slog.String("bravo", "3"),
		slog.String("yankee", "4"),
		slog.String("charlie", "5"),
	)

	var first string

	for i := range 50 {
		buf.Reset()

		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatalf("Handle() error = %v", err)
		}

		out := buf.String()
		if i == 0 {
			first = out
			continue
		}

		if out != first {
			t.Fatalf("render %d = %q, want byte-identical to %q", i, out, first)
		}
	}

	// AttrOrder still comes first, then the remaining keys sorted by name.
	wantOrder := []string{"[ordered]", "[alpha=1]", "[bravo=3]", "[charlie=5]", "[mike=2]", "[yankee=4]"}

	prev := -1

	for _, want := range wantOrder {
		idx := strings.Index(first, want)
		if idx < 0 {
			t.Fatalf("output %q missing %q", first, want)
		}

		if idx < prev {
			t.Fatalf("output %q out of order at %q", first, want)
		}

		prev = idx
	}
}
