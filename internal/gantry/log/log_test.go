// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNew_JSON(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf, "info", "json")
	l.Info("hello", slog.String("k", "v"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.String())
	}

	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("rec = %v", rec)
	}
}

func TestNew_Text(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf, "info", "text")
	l.Info("hello")

	out := buf.String()
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("text output missing msg: %q", out)
	}
}

func TestSubsystemTag(t *testing.T) {
	var buf bytes.Buffer

	l := Subsystem(New(&buf, "info", "json"), "cache")
	l.Info("evicted")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}

	if rec["subsystem"] != "cache" {
		t.Errorf("subsystem = %v, want cache", rec["subsystem"])
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"unknown": slog.LevelInfo, // fallback
		"":        slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
