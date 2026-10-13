// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

const thisPackage = "github.com/Azure/unbounded/internal/logger."

func parseFlags(t *testing.T, preset Options, args ...string) (Options, error) {
	t.Helper()

	opts := preset

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts.BindFlags(fs)
	opts.BindDebugFlag(fs)

	return opts, fs.Parse(args)
}

func handler(w io.Writer, level slog.Level, format Format) slog.Handler {
	return Options{Level: level, Format: format}.Handler(w)
}

// decodeLines parses one JSON record per line of out.
func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()

	var records []map[string]any

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "line %q", line)

		records = append(records, rec)
	}

	return records
}

// requireStackFrom checks that stack is in zap's layout and starts in the
// function named fn in this package's test file.
func requireStackFrom(t *testing.T, stack any, fn string) {
	t.Helper()

	s, ok := stack.(string)
	require.True(t, ok, "stacktrace is %T, want string", stack)

	lines := strings.Split(s, "\n")
	require.GreaterOrEqual(t, len(lines), 2, "stacktrace %q", s)
	require.True(t, strings.HasPrefix(lines[0], thisPackage+fn), "first frame %q, want %s%s", lines[0], thisPackage, fn)
	require.True(t, strings.HasPrefix(lines[1], "\t"), "file line %q", lines[1])
	require.Contains(t, lines[1], "setup_test.go:")
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug":     slog.LevelDebug,
		"INFO":      slog.LevelInfo,
		" warn\n":   slog.LevelWarn,
		"warning":   slog.LevelWarn,
		"Warning":   slog.LevelWarn,
		"error":     slog.LevelError,
		"info-1":    slog.Level(-1),
		"debug+3":   slog.Level(-1),
		"error+100": slog.LevelError + 100,
	} {
		got, err := ParseLevel(in)
		require.NoError(t, err, "ParseLevel(%q)", in)
		require.Equal(t, want, got, "ParseLevel(%q)", in)
	}

	for _, in := range []string{"", " ", "verbose", "info-x", "1", "warning-1"} {
		_, err := ParseLevel(in)
		require.ErrorContains(t, err, "unknown log level", "ParseLevel(%q)", in)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"text":         FormatText,
		"JSON":         FormatJSON,
		" prettytext ": FormatPrettyText,
		"PrettyText":   FormatPrettyText,
	} {
		got, err := ParseFormat(in)
		require.NoError(t, err, "ParseFormat(%q)", in)
		require.Equal(t, want, got, "ParseFormat(%q)", in)
	}

	for _, in := range []string{"", "yaml", "pretty", "console"} {
		_, err := ParseFormat(in)
		require.ErrorContains(t, err, "unknown log format", "ParseFormat(%q)", in)
	}
}

func TestBindFlagsDefaults(t *testing.T) {
	var opts Options

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.BindFlags(fs)

	require.Equal(t, DefaultLevel, opts.Level)
	require.Equal(t, FormatText, opts.Format)

	level := fs.Lookup("log-level")
	require.NotNil(t, level)
	require.Equal(t, "info", level.DefValue)
	require.Equal(t, "level", level.Value.Type())

	format := fs.Lookup("log-format")
	require.NotNil(t, format)
	require.Equal(t, "text", format.DefValue)
	require.Equal(t, "format", format.Value.Type())
}

// TestBindFlagsKeepsPresetFormat checks that a binary can choose its default
// format but not its default level, which comes from DefaultLevel.
func TestBindFlagsKeepsPresetFormat(t *testing.T) {
	opts := Options{Level: slog.LevelWarn, Format: FormatJSON, AddSource: true}

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.BindFlags(fs)

	require.NoError(t, fs.Parse(nil))
	require.Equal(t, Options{Level: DefaultLevel, Format: FormatJSON, AddSource: true}, opts)
	require.Equal(t, "info", fs.Lookup("log-level").DefValue)
	require.Equal(t, "json", fs.Lookup("log-format").DefValue)
}

func TestBindFlagsParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want Options
	}{
		{name: "defaults", want: Options{Level: DefaultLevel, Format: FormatText}},
		{name: "debug json", args: []string{"--log-level=debug", "--log-format=json"}, want: Options{Level: slog.LevelDebug, Format: FormatJSON}},
		{name: "case insensitive", args: []string{"--log-level=WARN", "--log-format=JSON"}, want: Options{Level: slog.LevelWarn, Format: FormatJSON}},
		{name: "warning alias", args: []string{"--log-level=warning"}, want: Options{Level: slog.LevelWarn, Format: FormatText}},
		{name: "offset enables V(1)", args: []string{"--log-level=info-1"}, want: Options{Level: slog.Level(-1), Format: FormatText}},
		{name: "prettytext", args: []string{"--log-format=prettytext"}, want: Options{Level: DefaultLevel, Format: FormatPrettyText}},
		{name: "debug alias", args: []string{"--debug"}, want: Options{Level: slog.LevelDebug, Format: FormatText}},
		{name: "debug alias false", args: []string{"--debug=false"}, want: Options{Level: DefaultLevel, Format: FormatText}},
		{name: "log-level after debug wins", args: []string{"--debug", "--log-level=warn"}, want: Options{Level: slog.LevelWarn, Format: FormatText}},
		{name: "debug after log-level wins", args: []string{"--log-level=warn", "--debug"}, want: Options{Level: slog.LevelDebug, Format: FormatText}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFlags(t, Options{}, tc.args...)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestBindFlagsRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown level", args: []string{"--log-level=verbose"}, want: "log-level"},
		{name: "empty level", args: []string{"--log-level="}, want: "log-level"},
		{name: "bad offset", args: []string{"--log-level=info-x"}, want: "log-level"},
		{name: "unknown format", args: []string{"--log-format=yaml"}, want: "log-format"},
		{name: "empty format", args: []string{"--log-format="}, want: "log-format"},
		{name: "debug not a bool", args: []string{"--debug=maybe"}, want: "debug"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(t, Options{}, tc.args...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestBindGoFlags(t *testing.T) {
	opts := Options{Format: FormatJSON}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts.BindGoFlags(fs)

	require.Equal(t, "info", fs.Lookup("log-level").DefValue)
	require.Equal(t, "json", fs.Lookup("log-format").DefValue)

	require.NoError(t, fs.Parse([]string{"-log-level=debug", "--log-format=text", "positional"}))
	require.Equal(t, Options{Level: slog.LevelDebug, Format: FormatText}, opts)
	require.Equal(t, []string{"positional"}, fs.Args())

	require.ErrorContains(t, fs.Parse([]string{"--log-level=loud"}), "unknown log level")
}

// TestBindGoFlagsHelpShowsDefaults: the flag package leaves a default out of
// the help text when it equals the zero value's String, which "info" did
// before levelValue held a pointer.
func TestBindGoFlagsHelpShowsDefaults(t *testing.T) {
	var (
		opts Options
		help bytes.Buffer
	)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&help)
	opts.BindGoFlags(fs)
	fs.PrintDefaults()

	require.Contains(t, help.String(), "(default info)")
	require.Contains(t, help.String(), "(default text)")
}

func TestLevelValueString(t *testing.T) {
	// slog names levels between Debug and Info relative to Debug, so V(1)
	// (level -1) renders as debug+3 even though info-1 parses to it.
	for level, want := range map[slog.Level]string{
		slog.LevelDebug: "debug",
		slog.LevelInfo:  "info",
		slog.Level(-1):  "debug+3",
		slog.LevelWarn:  "warn",
		slog.LevelError: "error",
	} {
		require.Equal(t, want, levelValue{&level}.String())

		var parsed slog.Level
		require.NoError(t, levelValue{&parsed}.Set(want))
		require.Equal(t, level, parsed, "round trip of %q", want)
	}

	require.Empty(t, levelValue{}.String(), "the zero value prints nothing")
}

func TestHandlerText(t *testing.T) {
	var buf bytes.Buffer

	slog.New(handler(&buf, slog.LevelInfo, FormatText)).Info("hello", "key", "value")

	out := buf.String()
	require.Contains(t, out, "level=INFO")
	require.Contains(t, out, "msg=hello")
	require.Contains(t, out, "key=value")
}

func TestHandlerEmptyFormatIsText(t *testing.T) {
	var buf bytes.Buffer

	slog.New(handler(&buf, slog.LevelInfo, "")).Info("hello")

	require.Contains(t, buf.String(), "msg=hello")
}

func TestHandlerJSON(t *testing.T) {
	var buf bytes.Buffer

	slog.New(handler(&buf, slog.LevelInfo, FormatJSON)).Info("hello", "key", "value")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	require.Equal(t, "INFO", rec["level"])
	require.Equal(t, "hello", rec["msg"])
	require.Equal(t, "value", rec["key"])
	require.NotContains(t, rec, slog.SourceKey)
}

func TestHandlerAddSource(t *testing.T) {
	var buf bytes.Buffer

	slog.New(Options{Level: slog.LevelInfo, Format: FormatJSON, AddSource: true}.Handler(&buf)).Info("hello")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))

	source, ok := rec[slog.SourceKey].(map[string]any)
	require.True(t, ok, "source is %T", rec[slog.SourceKey])
	require.Equal(t, thisPackage+"TestHandlerAddSource", source["function"])
	require.Contains(t, source["file"], "setup_test.go")
}

func TestHandlerLabelsSubInfoLevelsDebug(t *testing.T) {
	for level, want := range map[slog.Level]string{
		slog.Level(-8):  "DEBUG",
		slog.LevelDebug: "DEBUG",
		slog.Level(-1):  "DEBUG",
		slog.LevelInfo:  "INFO",
		slog.LevelWarn:  "WARN",
		slog.LevelError: "ERROR",
	} {
		var buf bytes.Buffer

		slog.New(handler(&buf, slog.Level(-8), FormatJSON)).Log(context.Background(), level, "probe", "level_attr", "kept")

		var rec map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
		require.Equal(t, want, rec["level"], "level %d", int(level))
		require.Equal(t, "kept", rec["level_attr"])
	}

	var buf bytes.Buffer

	logr.FromSlogHandler(handler(&buf, slog.LevelDebug, FormatText)).V(1).Info("verbose")
	require.Contains(t, buf.String(), "level=DEBUG ")
}

// TestLogrVerbosity checks that logr V(n) Info calls log at slog level -n and
// that Error always logs at ERROR.
func TestLogrVerbosity(t *testing.T) {
	cases := []struct {
		level      slog.Level
		maxEnabled int // highest V(n) that is logged; -1 means not even V(0)
	}{
		{level: slog.LevelError, maxEnabled: -1},
		{level: slog.LevelWarn, maxEnabled: -1},
		{level: slog.LevelInfo, maxEnabled: 0},
		{level: slog.Level(-1), maxEnabled: 1},
		{level: slog.Level(-2), maxEnabled: 2},
		{level: slog.LevelDebug, maxEnabled: 4},
	}

	for _, tc := range cases {
		t.Run(tc.level.String(), func(t *testing.T) {
			var buf bytes.Buffer

			log := logr.FromSlogHandler(handler(&buf, tc.level, FormatJSON))

			for v := 0; v <= 5; v++ {
				buf.Reset()
				log.V(v).Info("probe")

				logged := buf.Len() > 0
				require.Equal(t, v <= tc.maxEnabled, logged, "V(%d) at level %s", v, tc.level)
			}

			buf.Reset()
			log.Error(errors.New("boom"), "failed")

			var rec map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
			require.Equal(t, "ERROR", rec["level"])
			require.Equal(t, "boom", rec["err"])
		})
	}
}

func TestErrorStacktrace(t *testing.T) {
	var buf bytes.Buffer

	h := handler(&buf, slog.LevelDebug, FormatJSON)

	slog.New(h).Error("from slog")
	logr.FromSlogHandler(h).Error(errors.New("boom"), "from logr")
	slog.New(h).With("key", "value").Error("with attrs")
	logr.FromSlogHandler(h).WithValues("key", "value").WithName("named").Error(errors.New("boom"), "logr with values")

	records := decodeLines(t, buf.String())
	require.Len(t, records, 4)

	for _, rec := range records {
		requireStackFrom(t, rec[stacktraceKey], "TestErrorStacktrace")
	}

	require.Equal(t, "value", records[2]["key"])
	require.Equal(t, "value", records[3]["key"])
	require.Equal(t, "named", records[3]["logger"])
}

func TestNoStacktraceBelowError(t *testing.T) {
	var buf bytes.Buffer

	h := handler(&buf, slog.LevelDebug, FormatJSON)
	slog.New(h).Debug("debug")
	slog.New(h).Info("info")
	slog.New(h).Warn("warn")
	logr.FromSlogHandler(h).V(1).Info("verbose")

	records := decodeLines(t, buf.String())
	require.Len(t, records, 4)

	for _, rec := range records {
		require.NotContains(t, rec, stacktraceKey, "record %v", rec)
	}
}

func TestErrorStacktraceTextIsOneLine(t *testing.T) {
	var buf bytes.Buffer

	slog.New(handler(&buf, slog.LevelInfo, FormatText)).Error("failed")

	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "\n"), "output %q", out)
	require.Contains(t, out, ` stacktrace="`+thisPackage+`TestErrorStacktraceTextIsOneLine\n\t`)
}

func TestErrorStacktraceWithoutPC(t *testing.T) {
	var buf bytes.Buffer

	h := handler(&buf, slog.LevelInfo, FormatJSON)
	require.NoError(t, h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "no pc", 0)))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))

	stack, ok := rec[stacktraceKey].(string)
	require.True(t, ok)
	require.Contains(t, stack, thisPackage+"TestErrorStacktraceWithoutPC\n\t", "falls back to the whole stack")
}

// TestSetup installs the process-wide logger. controller-runtime accepts only
// the first logger it is given and offers no way to reset it, so this must
// remain the only test in the package that calls Setup.
func TestSetup(t *testing.T) {
	previous := slog.Default()

	t.Cleanup(func() { slog.SetDefault(previous) })

	var buf bytes.Buffer

	got := Options{Level: slog.Level(-1), Format: FormatJSON}.Setup(&buf)
	require.Same(t, got, slog.Default())

	slog.Info("from slog")
	crlog.Log.WithName("ctrl").Info("from controller-runtime", "key", "value")
	crlog.Log.V(1).Info("verbose enabled")
	crlog.Log.V(2).Info("verbose suppressed")
	crlog.Log.WithName("ctrl").Error(errors.New("boom"), "controller-runtime error")

	records := decodeLines(t, buf.String())
	require.Len(t, records, 4)
	require.Equal(t, "from slog", records[0]["msg"])
	require.Equal(t, "from controller-runtime", records[1]["msg"])
	require.Equal(t, "ctrl", records[1]["logger"])
	require.Equal(t, "value", records[1]["key"])
	require.Equal(t, "verbose enabled", records[2]["msg"])
	require.Equal(t, "DEBUG", records[2]["level"])
	require.Equal(t, "controller-runtime error", records[3]["msg"])

	// controller-runtime's delegating logger adds frames of its own; the stack
	// must still start at this call.
	requireStackFrom(t, records[3][stacktraceKey], "TestSetup")
}
