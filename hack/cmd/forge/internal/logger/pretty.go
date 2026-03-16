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
)

// consoleHandler is a slog.Handler implementation that writes log records
// to stdout ensuring a deterministic ordering of selected attributes. The order
// is defined at construction time via attrOrder. Only attributes whose keys are
// present in attrOrder are rendered (and rendered exactly once) in the provided
// order. Additional attributes (not listed in attrOrder) are ignored to keep
// output stable and predictable.
//
// A line is formatted as:
//
//	<LEVEL> <timestamp> [attr1] [attr2] ... [attrN] <message>
//
// Where:
//   - <timestamp> is RFC3339 and logically associated with the key
//     PreciseTimeStamp (not printed, per requirement to omit keys).
//   - Only attribute VALUES (no key names) are printed, in the order supplied
//     at construction time. Missing attributes are skipped silently.
//   - The original log message text appears last.
type consoleHandler struct {
	mu           *sync.Mutex  // protects writes + base state copies
	attrOrder    []string     // desired attribute key order
	baseAttrs    []slog.Attr  // attributes added via WithAttrs / With
	groups       []string     // group stack (kept for interface parity)
	w            io.Writer    // destination (defaults to os.Stderr)
	level        slog.Leveler // optional level filter
	enableColors bool
	attrColors   map[string]color.Attribute
}

//var colorScheme = map[string]color.Attribute{
//	"field":                  color.FgCyan,
//	slog.LevelDebug.String(): color.FgHiBlack,
//	slog.LevelWarn.String():  color.FgYellow,
//	slog.LevelInfo.String():  color.FgGreen,
//	slog.LevelError.String(): color.FgRed,
//}

type PrettyFieldHandlerOptions struct {
	Level      slog.Leveler
	AttrOrder  []string
	AttrColors map[string]color.Attribute
}

// NewPrettyFieldHandler creates a new slog.Handler which writes to stderr
// and prints the provided ordered attribute keys after the log message.
func NewPrettyFieldHandler(lvl slog.Leveler, opts PrettyFieldHandlerOptions) slog.Handler {
	if lvl == nil {
		// Default to Info level filtering if none provided.
		lv := new(slog.LevelVar)
		lv.Set(slog.LevelInfo)
		lvl = lv
	}

	color.NoColor = false // force enable colors

	if opts.AttrColors == nil {
		opts.AttrColors = map[string]color.Attribute{}
	}

	// log levels have specific colors unless overridden in AttrColors
	if _, ok := opts.AttrColors[slog.LevelDebug.String()]; !ok {
		opts.AttrColors[slog.LevelDebug.String()] = color.FgHiBlack
	}

	if _, ok := opts.AttrColors[slog.LevelWarn.String()]; !ok {
		opts.AttrColors[slog.LevelWarn.String()] = color.FgYellow
	}

	if _, ok := opts.AttrColors[slog.LevelInfo.String()]; !ok {
		opts.AttrColors[slog.LevelInfo.String()] = color.FgGreen
	}

	if _, ok := opts.AttrColors[slog.LevelError.String()]; !ok {
		opts.AttrColors[slog.LevelError.String()] = color.FgRed
	}

	// default color when nothing is defined
	if _, ok := opts.AttrColors[""]; !ok {
		opts.AttrColors[""] = color.FgHiBlue
	}

	return &consoleHandler{
		mu:           &sync.Mutex{},
		attrOrder:    opts.AttrOrder,
		w:            os.Stderr,
		level:        lvl,
		enableColors: true,
		attrColors:   opts.AttrColors,
	}
}

// Enabled reports whether the handler handles records at the given level.
func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.level == nil {
		return true
	}

	return level >= h.level.Level()
}

// Handle formats and writes the record honoring the attribute order.
func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) { // fast path
		return nil
	}

	// Collect attribute values into a map for quick lookup. Latest wins.
	attrMap := make(map[string]any, len(h.baseAttrs)+r.NumAttrs())

	// Base attributes first.
	for _, a := range h.baseAttrs {
		if a.Value.Kind() != slog.KindGroup { // ignore groups for simplicity
			attrMap[a.Key] = a.Value.Any()
		}
	}

	// Record attributes override base ones.
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() != slog.KindGroup {
			attrMap[a.Key] = a.Value.Any()
		}

		return true
	})

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
		if v, ok := attrMap[k]; ok {
			buf.WriteString(h.colorize(fmt.Sprintf("[%s]", v), h.getFieldColor(k)))

			// Mark this key as added to avoid duplication later.
			attrAdded[k] = struct{}{}
		}
	}

	if r.Message != "" {
		buf.WriteByte(' ')
		buf.WriteString(h.colorize(r.Message, h.getLevelColor(r.Level)))
	}

	// Append any additional attributes that were not in attrOrder, to ensure all attributes are printed.
	for k, v := range attrMap {
		if _, alreadyAdded := attrAdded[k]; !alreadyAdded {
			buf.WriteString(h.colorize(fmt.Sprintf(" [%s=%v]", k, v), h.getFieldColor(k)))
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
		groups:       append([]string{}, h.groups...),
		w:            h.w,
		level:        h.level,
		enableColors: h.enableColors, // preserve color setting when cloning
		attrColors:   h.attrColors,
	}
}

// WithGroup returns a new handler with an additional group. Grouping is
// preserved but not rendered (kept for interface compliance). Keys inside the
// group still use their raw names for ordering purposes.
func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" { // no-op
		return h
	}

	nh := &consoleHandler{
		mu:           h.mu, // share parent's mutex
		attrOrder:    append([]string{}, h.attrOrder...),
		baseAttrs:    append([]slog.Attr{}, h.baseAttrs...),
		groups:       append(append([]string{}, h.groups...), name),
		w:            h.w,
		level:        h.level,
		enableColors: h.enableColors, // preserve color setting when cloning
		attrColors:   h.attrColors,
	}

	return nh
}

func (h *consoleHandler) colorize(v string, colorCode color.Attribute) string {
	if h.enableColors {
		v = color.New(colorCode).Sprint(v)
	}

	return v
}

func (h *consoleHandler) getFieldColor(field string) color.Attribute {
	if c, ok := h.attrColors[field]; ok {
		return c
	}

	return h.attrColors[""] // default color
}

func (h *consoleHandler) getLevelColor(level slog.Level) color.Attribute {
	return h.getFieldColor(level.String())
}
