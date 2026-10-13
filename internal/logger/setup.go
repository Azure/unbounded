// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package logger sets up log/slog for the project's Go binaries.
//
// Every binary configures logging here. Options.BindFlags, or BindGoFlags for
// the standard flag package, registers --log-level and --log-format, and
// Options.Setup installs the handler as the slog default and as
// controller-runtime's logger. Components that also read a level or format
// from an environment variable or config file parse it with ParseLevel and
// ParseFormat.
package logger

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultLevel is the log level every binary uses unless configured otherwise.
const DefaultLevel = slog.LevelInfo

// stacktraceKey is the attribute text and JSON handlers add to error records.
// It matches the key zap used.
const stacktraceKey = "stacktrace"

const (
	levelUsage = "Minimum log level: debug, info, warn or error, with an optional offset " +
		"(info-1 enables logr V(1); debug enables V(1) through V(4))"
	formatUsage = "Log output format: text, json or prettytext"
)

// Format selects how log records are encoded.
type Format string

const (
	// FormatText writes logfmt-style key=value lines (slog.TextHandler).
	FormatText Format = "text"
	// FormatJSON writes one JSON object per line (slog.JSONHandler).
	FormatJSON Format = "json"
	// FormatPrettyText writes human-oriented lines for command-line tools,
	// colored on a terminal. See consoleHandler.
	FormatPrettyText Format = "prettytext"
)

// ParseFormat parses a format name, ignoring case and surrounding space.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case FormatText, FormatJSON, FormatPrettyText:
		return f, nil
	default:
		return "", fmt.Errorf("unknown log format %q: must be %s, %s or %s", s, FormatText, FormatJSON, FormatPrettyText)
	}
}

// ParseLevel parses a level name, ignoring case and surrounding space. It
// accepts debug, info, warn and error, each with an optional integer offset
// such as info-1, and warning as another name for warn.
func ParseLevel(s string) (slog.Level, error) {
	trimmed := strings.TrimSpace(s)
	if strings.EqualFold(trimmed, "warning") {
		return slog.LevelWarn, nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(trimmed)); err != nil {
		return 0, fmt.Errorf("unknown log level %q: must be debug, info, warn or error, optionally with an offset such as info-1", s)
	}

	return level, nil
}

// String implements flag.Value and pflag.Value.
func (f *Format) String() string { return string(*f) }

// Set implements flag.Value and pflag.Value.
func (f *Format) Set(s string) error {
	parsed, err := ParseFormat(s)
	if err != nil {
		return err
	}

	*f = parsed

	return nil
}

// Type implements pflag.Value.
func (f *Format) Type() string { return "format" }

// levelValue adapts a *slog.Level to flag.Value and pflag.Value.
//
// It holds a pointer so its zero value prints as "", not "info". The flag
// package leaves a default out of the help text when it matches the zero
// value's String, which would hide "(default info)".
type levelValue struct{ level *slog.Level }

func (v levelValue) String() string {
	if v.level == nil {
		return ""
	}

	return strings.ToLower(v.level.String())
}

func (v levelValue) Set(s string) error {
	level, err := ParseLevel(s)
	if err != nil {
		return err
	}

	*v.level = level

	return nil
}

func (v levelValue) Type() string { return "level" }

// debugValue backs the legacy boolean --debug flag: true sets the level to
// debug and false leaves it alone.
type debugValue slog.Level

func (v *debugValue) String() string { return strconv.FormatBool(slog.Level(*v) <= slog.LevelDebug) }

func (v *debugValue) Set(s string) error {
	enabled, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}

	if enabled {
		*v = debugValue(slog.LevelDebug)
	}

	return nil
}

func (v *debugValue) Type() string { return "bool" }

// Options is a binary's logging configuration.
type Options struct {
	// Level is the minimum level logged. BindFlags and BindGoFlags reset it to
	// DefaultLevel, so DefaultLevel is the one place every binary's default
	// level is set.
	Level slog.Level
	// Format selects the encoding. Empty means text.
	Format Format
	// AddSource adds the logging call's file and line to text and JSON records.
	AddSource bool
	// AttrOrder lists attributes prettytext prints, values only, before the
	// message.
	AttrOrder []string
	// NoColor turns off prettytext's colors. Without it, prettytext colors only
	// when writing to a terminal, NO_COLOR is unset or empty, and TERM is not
	// dumb.
	NoColor bool
}

// BindFlags registers --log-level and --log-format on fs, bound to o. The level
// defaults to DefaultLevel. The format defaults to o.Format, or text when it is
// empty, so a binary sets Format first to choose a different default.
func (o *Options) BindFlags(fs *pflag.FlagSet) {
	o.setDefaults()
	fs.Var(levelValue{&o.Level}, "log-level", levelUsage)
	fs.Var(&o.Format, "log-format", formatUsage)
}

// BindGoFlags is BindFlags for the standard library's flag package.
func (o *Options) BindGoFlags(fs *flag.FlagSet) {
	o.setDefaults()
	fs.Var(levelValue{&o.Level}, "log-level", levelUsage)
	fs.Var(&o.Format, "log-format", formatUsage)
}

// BindDebugFlag registers the legacy boolean --debug, which sets the level to
// debug. When both --debug and --log-level are given, the later one wins. Only
// for binaries that already accepted --debug; the caller hides it.
func (o *Options) BindDebugFlag(fs *pflag.FlagSet) {
	fs.Var((*debugValue)(&o.Level), "debug", "Same as --log-level=debug")
	fs.Lookup("debug").NoOptDefVal = "true"
}

func (o *Options) setDefaults() {
	o.Level = DefaultLevel

	if o.Format == "" {
		o.Format = FormatText
	}
}

// Setup builds the handler for o, writing to w, and installs it for log/slog,
// the standard log package and controller-runtime. It returns the slog.Logger
// wrapping it.
//
// Call it once: controller-runtime ignores every logger after the first.
func (o Options) Setup(w io.Writer) *slog.Logger {
	h := o.Handler(w)
	l := slog.New(h)

	slog.SetDefault(l)
	crlog.SetLogger(logr.FromSlogHandler(h))

	return l
}

// Handler returns the handler for o, writing to w, without installing it.
//
// Text and JSON print every level below info as DEBUG, and add a stacktrace
// attribute, starting at the logging call, to records at error level. Any
// format other than json and prettytext is text.
func (o Options) Handler(w io.Writer) slog.Handler {
	if o.Format == FormatPrettyText {
		return newPrettyHandler(w, o.Level, o.AttrOrder, !o.NoColor && colorsFor(w))
	}

	opts := &slog.HandlerOptions{Level: o.Level, AddSource: o.AddSource, ReplaceAttr: debugLevelLabel}

	var h slog.Handler
	if o.Format == FormatJSON {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}

	return stackHandler{h}
}

// debugLevelLabel prints every level below info as DEBUG. logr V(n) logs at
// slog level -n, which slog would otherwise print as DEBUG+3 for V(1).
func debugLevelLabel(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 || a.Key != slog.LevelKey {
		return a
	}

	if level, ok := a.Value.Any().(slog.Level); ok && level < slog.LevelInfo {
		a.Value = slog.StringValue(slog.LevelDebug.String())
	}

	return a
}

// stackHandler adds a stacktrace attribute to records at error level and
// above.
type stackHandler struct {
	slog.Handler
}

func (h stackHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		r = r.Clone()
		r.AddAttrs(slog.String(stacktraceKey, stacktrace(r.PC)))
	}

	return h.Handler.Handle(ctx, r)
}

func (h stackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return stackHandler{h.Handler.WithAttrs(attrs)}
}

func (h stackHandler) WithGroup(name string) slog.Handler {
	return stackHandler{h.Handler.WithGroup(name)}
}

// stacktrace formats the current goroutine's stack in zap's layout, one
// "function\n\tfile:line" pair per frame. It starts at pc, the program counter
// slog and logr record for the logging call, so the logging library's own
// frames are left out. If pc is not on the stack, the whole stack is used.
func stacktrace(pc uintptr) string {
	pcs := make([]uintptr, 64)

	for {
		n := runtime.Callers(2, pcs)
		if n < len(pcs) {
			pcs = pcs[:n]

			break
		}

		pcs = make([]uintptr, 2*len(pcs))
	}

	if i := slices.Index(pcs, pc); i >= 0 {
		pcs = pcs[i:]
	}

	var b strings.Builder

	frames := runtime.CallersFrames(pcs)

	for {
		frame, more := frames.Next()
		if frame.Function != "" || frame.File != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}

			fmt.Fprintf(&b, "%s\n\t%s:%d", frame.Function, frame.File, frame.Line)
		}

		if !more {
			return b.String()
		}
	}
}
