// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/fatih/color"
	"golang.org/x/term"
)

// consoleHandler is the prettytext slog.Handler. It writes one line per record:
//
//	<timestamp> [<L>] [v1][v2]... <message> [key=value] ...
//
// <timestamp> is RFC3339 in UTC and <L> is the first letter of the level.
// Values of attributes named in attrOrder come next, in that order and
// without their keys. Every other attribute follows the message as
// [key=value], in the order it was added: logger attributes first, then the
// call's. A repeated key keeps its first position and shows the newest value.
// Group attributes are not printed.
type consoleHandler struct {
	mu           *sync.Mutex  // protects writes + base state copies
	attrOrder    []string     // desired attribute key order
	baseAttrs    []slog.Attr  // attributes added via WithAttrs / With
	w            io.Writer    // destination
	level        slog.Leveler // level filter
	enableColors bool
	attrColors   map[string]color.Attribute
}

// newPrettyHandler returns the prettytext handler, writing records at or above
// level to w. With colors set, it colors the output regardless of the color
// package's process-wide setting.
func newPrettyHandler(w io.Writer, level slog.Leveler, attrOrder []string, colors bool) slog.Handler {
	return &consoleHandler{
		mu:           &sync.Mutex{},
		attrOrder:    attrOrder,
		w:            w,
		level:        level,
		enableColors: colors,
		attrColors: map[string]color.Attribute{
			slog.LevelDebug.String(): color.FgHiBlack,
			slog.LevelInfo.String():  color.FgGreen,
			slog.LevelWarn.String():  color.FgYellow,
			slog.LevelError.String(): color.FgRed,
			"":                       color.FgHiBlue, // everything else
		},
	}
}

// colorsFor reports whether prettytext output to w should be colored: w is a
// terminal, NO_COLOR is unset or empty, and TERM is not dumb.
func colorsFor(w io.Writer) bool {
	f, ok := w.(*os.File)

	return colorsAllowed(ok && term.IsTerminal(int(f.Fd())), os.Getenv)
}

func colorsAllowed(terminal bool, getenv func(string) string) bool {
	return terminal && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// Enabled reports whether the handler handles records at the given level.
func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// prettyAttr is one attribute to print, in first-added order.
type prettyAttr struct {
	key   string
	value any
}

// Handle formats and writes the record honoring the attribute order.
func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) { // fast path
		return nil
	}

	// Collect attributes in the order they were added. A repeated key keeps
	// its first position and takes the newest value.
	attrs := make([]prettyAttr, 0, len(h.baseAttrs)+r.NumAttrs())
	index := make(map[string]int, cap(attrs))

	add := func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindGroup { // ignore groups for simplicity
			return true
		}

		if i, ok := index[a.Key]; ok {
			attrs[i].value = a.Value.Any()

			return true
		}

		index[a.Key] = len(attrs)
		attrs = append(attrs, prettyAttr{key: a.Key, value: a.Value.Any()})

		return true
	}

	for _, a := range h.baseAttrs {
		add(a)
	}

	r.Attrs(add)

	// Build the line.
	var buf bytes.Buffer

	// Timestamp (PreciseTimeStamp). Use record.Time if provided.
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	buf.WriteString(ts.UTC().Format(time.RFC3339))
	buf.WriteByte(' ')

	levelLetter := string(r.Level.String()[0]) // first letter of the level
	buf.WriteString(h.colorize(fmt.Sprintf("[%s] ", levelLetter), h.getLevelColor(r.Level)))

	attrAdded := map[string]struct{}{}

	// Append ordered attribute VALUES only, each wrapped in square brackets.
	for _, k := range h.attrOrder {
		if i, ok := index[k]; ok {
			buf.WriteString(h.colorize(fmt.Sprintf("[%s]", attrs[i].value), h.getFieldColor(k)))

			// Mark this key as added to avoid duplication later.
			attrAdded[k] = struct{}{}
		}
	}

	if r.Message != "" {
		buf.WriteByte(' ')
		buf.WriteString(h.colorize(r.Message, h.getLevelColor(r.Level)))
	}

	// Append the attributes not in attrOrder, so every attribute is printed.
	for _, a := range attrs {
		if _, alreadyAdded := attrAdded[a.key]; !alreadyAdded {
			buf.WriteString(h.colorize(fmt.Sprintf(" [%s=%v]", a.key, a.value), h.getFieldColor(a.key)))
		}
	}

	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.w.Write(buf.Bytes())

	return err
}

// WithAttrs returns a new handler with the provided attributes added. These
// can later be printed if their keys are in attrOrder.
func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &consoleHandler{
		mu:           h.mu, // share parent's mutex to synchronize all descendants
		attrOrder:    append([]string{}, h.attrOrder...),
		baseAttrs:    append(append([]slog.Attr{}, h.baseAttrs...), attrs...),
		w:            h.w,
		level:        h.level,
		enableColors: h.enableColors, // preserve color setting when cloning
		attrColors:   h.attrColors,
	}
}

// WithGroup returns h. Groups are not rendered, and keys inside a group keep
// their own names for ordering.
func (h *consoleHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *consoleHandler) colorize(v string, colorCode color.Attribute) string {
	if !h.enableColors {
		return v
	}

	c := color.New(colorCode)
	c.EnableColor() // the handler decided already; ignore color.NoColor

	return c.Sprint(v)
}

func (h *consoleHandler) getFieldColor(field string) color.Attribute {
	if c, ok := h.attrColors[field]; ok {
		return c
	}

	return h.attrColors[""] // default color
}

// getLevelColor colors every level below info, such as logr V(1), as debug.
func (h *consoleHandler) getLevelColor(level slog.Level) color.Attribute {
	if level < slog.LevelInfo {
		level = slog.LevelDebug
	}

	return h.getFieldColor(level.String())
}
