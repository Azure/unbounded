// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fatih/color"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// prettyLogger writes prettytext to buf, which is not a terminal, so the
// output is never colored.
func prettyLogger(buf *bytes.Buffer, level slog.Level, attrOrder ...string) *slog.Logger {
	return slog.New(Options{Level: level, Format: FormatPrettyText, AttrOrder: attrOrder}.Handler(buf))
}

func TestPrettyTextLayout(t *testing.T) {
	var buf bytes.Buffer

	prettyLogger(&buf, slog.LevelInfo, "task", "stage").
		With("task", "download").
		Info("started", "stage", "fetch", "size", 3)

	line := buf.String()
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z \[I\] \[download\]\[fetch\] started \[size=3\]\n$`, line)
}

// TestPrettyTextAttributeOrder: attributes not in AttrOrder print in the order
// they were added, so the same call always prints the same line.
func TestPrettyTextAttributeOrder(t *testing.T) {
	var first string

	for range 20 {
		var buf bytes.Buffer

		prettyLogger(&buf, slog.LevelInfo).
			With("b", 1, "a", 2).
			Info("msg", "d", 3, "c", 4, "a", 5)

		_, attrs, ok := strings.Cut(buf.String(), " msg")
		require.True(t, ok, "line %q", buf.String())

		// A repeated key keeps its first position and shows the newest value.
		require.Equal(t, " [b=1] [a=5] [d=3] [c=4]\n", attrs)

		if first == "" {
			first = attrs
		}

		require.Equal(t, first, attrs)
	}
}

func TestPrettyTextHonorsLevel(t *testing.T) {
	var buf bytes.Buffer

	log := prettyLogger(&buf, slog.LevelInfo)
	log.Debug("hidden")
	require.Empty(t, buf.String())

	buf.Reset()
	log = prettyLogger(&buf, slog.LevelDebug)
	log.Debug("shown")
	logr.FromSlogHandler(log.Handler()).V(1).Info("verbose")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)
	require.Regexp(t, `\[D\] +shown$`, lines[0])
	require.Regexp(t, `\[D\] +verbose$`, lines[1])
}

func TestPrettyTextErrorHasNoStacktrace(t *testing.T) {
	var buf bytes.Buffer

	logr.FromSlogHandler(prettyLogger(&buf, slog.LevelInfo).Handler()).Error(errors.New("boom"), "failed")

	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "\n"), "one line: %q", out)
	require.Regexp(t, `\[E\] +failed \[err=boom\]\n$`, out)
	require.NotContains(t, out, stacktraceKey)
}

func TestPrettyTextPlainWhenNotATerminal(t *testing.T) {
	var buf bytes.Buffer

	prettyLogger(&buf, slog.LevelInfo).Info("plain")
	require.NotRegexp(t, ansi, buf.String())

	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	require.NoError(t, err)

	defer func() { require.NoError(t, file.Close()) }()

	require.False(t, colorsFor(&buf), "buffer")
	require.False(t, colorsFor(file), "regular file")
}

func TestColorsAllowed(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		env      map[string]string
		want     bool
	}{
		{name: "terminal", terminal: true, want: true},
		{name: "not a terminal", terminal: false},
		{name: "NO_COLOR set", terminal: true, env: map[string]string{"NO_COLOR": "1"}},
		{name: "NO_COLOR empty", terminal: true, env: map[string]string{"NO_COLOR": ""}, want: true},
		{name: "dumb terminal", terminal: true, env: map[string]string{"TERM": "dumb"}},
		{name: "xterm", terminal: true, env: map[string]string{"TERM": "xterm-256color"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, colorsAllowed(tc.terminal, func(k string) string { return tc.env[k] }))
		})
	}
}

// TestPrettyTextColors covers the colored output itself. It leaves the color
// package's process-wide setting alone and ignores it.
func TestPrettyTextColors(t *testing.T) {
	previous := color.NoColor

	t.Cleanup(func() { color.NoColor = previous })

	color.NoColor = true

	var buf bytes.Buffer

	slog.New(newPrettyHandler(&buf, slog.LevelInfo, nil, true)).Info("colored")
	require.Regexp(t, ansi, buf.String())
	require.Equal(t, "colored", strings.Fields(ansi.ReplaceAllString(buf.String(), ""))[2])
	require.True(t, color.NoColor, "color.NoColor changed")
}
