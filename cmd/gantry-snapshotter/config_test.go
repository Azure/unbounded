// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
)

// viable sets what every accepted configuration has to say: the node name, the
// way the DaemonSet supplies it through the Downward API rather than a flag,
// and the declaration that this daemon is alone on its volume, which a test
// without a membership view has to make for the same reason an operator does.
func viable(t *testing.T) {
	t.Helper()
	t.Setenv("GANTRY_NODE_NAME", "test-node")
	t.Setenv("GANTRY_SNAPSHOTTER_SINGLE_NODE", "true")
}

func TestParseConfigDefaults(t *testing.T) {
	viable(t)

	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.Socket != DefaultSocket {
		t.Errorf("socket = %q, want %q", cfg.Socket, DefaultSocket)
	}

	if cfg.Root != DefaultRoot {
		t.Errorf("root = %q, want %q", cfg.Root, DefaultRoot)
	}

	if cfg.SocketMode != DefaultSocketMode {
		t.Errorf("socket mode = %o, want %o", cfg.SocketMode, DefaultSocketMode)
	}

	if !cfg.AdoptSegments {
		t.Error("adopt-segments should default on")
	}

	if cfg.FormatCatalog {
		t.Error("format-catalog should default off: formatting is destructive")
	}

	if cfg.IngestWorkers != ingest.DefaultWorkers {
		t.Errorf("ingest workers = %d, want %d", cfg.IngestWorkers, ingest.DefaultWorkers)
	}

	if cfg.MountOptions != nil {
		t.Errorf("mount options = %v, want none", cfg.MountOptions)
	}
}

func TestParseConfigFlags(t *testing.T) {
	viable(t)

	cfg, err := parseConfig([]string{
		"-socket", "/tmp/x.sock",
		"-socket-mode", "0600",
		"-root", "/var/tmp/root",
		"-mount-options", "index=off, userxattr ,,",
		"-ingest-workers", "4",
		"-catalog-sync", "90s",
		"-format-catalog",
		"-adopt-segments=false",
		"-segment-blocks", "8",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.Socket != "/tmp/x.sock" {
		t.Errorf("socket = %q", cfg.Socket)
	}

	if cfg.SocketMode != 0o600 {
		t.Errorf("socket mode = %o, want 600", cfg.SocketMode)
	}

	if !slices.Equal(cfg.MountOptions, []string{"index=off", "userxattr"}) {
		t.Errorf("mount options = %v", cfg.MountOptions)
	}

	if cfg.IngestWorkers != 4 {
		t.Errorf("ingest workers = %d", cfg.IngestWorkers)
	}

	if cfg.CatalogSync != 90*time.Second {
		t.Errorf("catalog sync = %s", cfg.CatalogSync)
	}

	if !cfg.FormatCatalog || cfg.AdoptSegments || cfg.SegmentBlocks != 8 {
		t.Errorf("format=%v adopt=%v blocks=%d", cfg.FormatCatalog, cfg.AdoptSegments, cfg.SegmentBlocks)
	}
}

func TestParseConfigEnvironment(t *testing.T) {
	t.Setenv("GANTRY_SNAPSHOTTER_SOCKET", "/env/sock")
	t.Setenv("GANTRY_NODE_NAME", "node-7")
	t.Setenv("GANTRY_SNAPSHOTTER_SINGLE_NODE", "true")
	t.Setenv("GANTRY_SNAPSHOTTER_INGEST_DEPTH", "12")
	t.Setenv("GANTRY_SNAPSHOTTER_SKIP_VERIFY", "true")
	t.Setenv("GANTRY_SNAPSHOTTER_CLEANUP_INTERVAL", "3m")

	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.Socket != "/env/sock" || cfg.NodeName != "node-7" {
		t.Errorf("socket = %q node = %q", cfg.Socket, cfg.NodeName)
	}

	if cfg.IngestDepth != 12 || !cfg.SkipVerify || cfg.CleanupInterval != 3*time.Minute {
		t.Errorf("depth=%d verify=%v cleanup=%s", cfg.IngestDepth, cfg.SkipVerify, cfg.CleanupInterval)
	}
}

func TestFlagsBeatTheEnvironment(t *testing.T) {
	viable(t)

	t.Setenv("GANTRY_SNAPSHOTTER_SOCKET", "/env/sock")

	cfg, err := parseConfig([]string{"-socket", "/flag/sock"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.Socket != "/flag/sock" {
		t.Errorf("socket = %q, want the flag to win", cfg.Socket)
	}
}

// A malformed environment value must not stop the daemon: the value is
// ignored and the default stands.
func TestMalformedEnvironmentFallsBack(t *testing.T) {
	viable(t)

	t.Setenv("GANTRY_SNAPSHOTTER_INGEST_DEPTH", "lots")
	t.Setenv("GANTRY_SNAPSHOTTER_SKIP_VERIFY", "perhaps")
	t.Setenv("GANTRY_SNAPSHOTTER_CATALOG_SYNC", "soon")
	t.Setenv("GANTRY_SNAPSHOTTER_HOLE_GRACE", "eventually")
	t.Setenv("GANTRY_SNAPSHOTTER_WORK_HEADROOM", "plenty")

	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.IngestDepth != ingest.DefaultQueueDepth || cfg.SkipVerify || cfg.CatalogSync != DefaultCatalogSync {
		t.Errorf("depth=%d verify=%v sync=%s", cfg.IngestDepth, cfg.SkipVerify, cfg.CatalogSync)
	}

	if cfg.HoleGrace != catalog.DefaultHoleGrace {
		t.Errorf("hole grace = %s, want %s", cfg.HoleGrace, catalog.DefaultHoleGrace)
	}

	if cfg.WorkHeadroom != ingest.DefaultHeadroom {
		t.Errorf("work headroom = %d, want %d", cfg.WorkHeadroom, uint64(ingest.DefaultHeadroom))
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string

		// unnamed drops the node name the other cases take for
		// granted, for the cases that are about the name itself.
		unnamed bool
	}{
		{name: "positional", args: []string{"serve"}, want: "unexpected argument"},
		{name: "socket mode", args: []string{"-socket-mode", "notoctal"}, want: "socket-mode"},
		{name: "socket mode base", args: []string{"-socket-mode", "0899"}, want: "socket-mode"},
		{name: "segment blocks zero", args: []string{"-segment-blocks", "0"}, want: "segment-blocks"},
		{name: "segment blocks huge", args: []string{"-segment-blocks", "99999"}, want: "segment-blocks"},
		{name: "empty socket", args: []string{"-socket", ""}, want: "socket required"},
		{name: "empty root", args: []string{"-root", ""}, want: "root required"},
		{name: "empty work dir", args: []string{"-work-dir", ""}, want: "work-dir required"},
		{name: "empty devices", args: []string{"-devices", ""}, want: "devices required"},
		{name: "empty map root", args: []string{"-map-root", ""}, want: "map-root required"},
		{name: "no workers", args: []string{"-ingest-workers", "0"}, want: "ingest-workers"},
		{name: "no depth", args: []string{"-ingest-depth", "0"}, want: "ingest-depth"},
		{name: "no node name", want: "node-name required", unnamed: true},
		{name: "empty node name", args: []string{"-node-name", ""}, want: "node-name required"},
		{name: "members without node", args: []string{"-members-selector", "app=gantry"}, want: "node-name required", unnamed: true},
		{name: "clean without a member view", args: []string{"-node-name", "n1"}, want: "clean requires members-selector", unnamed: true},
		{name: "single node with members", args: []string{"-members-selector", "app=gantry"}, want: "contradict"},
		{name: "watermark grace zero", args: []string{"-watermark-grace", "0"}, want: "watermark-grace"},
		{name: "watermark grace negative", args: []string{"-watermark-grace", "-1m"}, want: "watermark-grace"},
		{name: "log level", args: []string{"-log-level", "chatty"}, want: "log-level"},
		{name: "log format", args: []string{"-log-format", "yaml"}, want: "log-format"},
		{name: "unknown flag", args: []string{"-nope"}, want: "not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.unnamed {
				viable(t)
			}

			var out strings.Builder

			_, err := parseConfig(tc.args, &out)
			if err == nil {
				t.Fatal("want an error")
			}

			if !strings.Contains(err.Error(), tc.want) && !strings.Contains(out.String(), tc.want) {
				t.Errorf("error %v / output %q, want %q", err, out.String(), tc.want)
			}
		})
	}
}

// A membership selector with a node name is the normal cluster configuration.
func TestMembersSelectorWithNodeName(t *testing.T) {
	cfg, err := parseConfig([]string{"-members-selector", "app=gantry", "-node-name", "n1"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.MembersSelector != "app=gantry" || cfg.NodeName != "n1" {
		t.Errorf("selector=%q node=%q", cfg.MembersSelector, cfg.NodeName)
	}
}

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"DEBUG", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
	} {
		got, err := parseLevel(tc.in)
		if err != nil {
			t.Fatalf("parseLevel(%q): %v", tc.in, err)
		}

		if got != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	if _, err := parseLevel("loud"); err == nil {
		t.Error("want an error for an unknown level")
	}
}

func TestSplitOptions(t *testing.T) {
	if got := splitOptions(""); got != nil {
		t.Errorf("empty = %v, want nil", got)
	}

	if got := splitOptions(" , ,, "); got != nil {
		t.Errorf("separators only = %v, want nil", got)
	}

	if got := splitOptions("a, b ,c"); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
}

func TestListen(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Socket: filepath.Join(dir, "nested", "s.sock"), SocketMode: 0o660}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	info, err := os.Stat(cfg.Socket)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Mode().Perm() != 0o660 {
		t.Errorf("mode = %o, want 660", info.Mode().Perm())
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A socket left behind by a previous run must not stop the daemon coming back.
func TestListenReplacesAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Socket: filepath.Join(dir, "s.sock"), SocketMode: 0o600}

	first, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Close the listener but leave the socket file, which is what a
	// SIGKILLed daemon leaves behind.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := os.WriteFile(cfg.Socket, nil, 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	second, err := listen(cfg)
	if err != nil {
		t.Fatalf("relisten: %v", err)
	}

	defer second.Close() //nolint:errcheck // test cleanup
}

func TestListenRejectsAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")

	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &Config{Socket: filepath.Join(blocker, "s.sock"), SocketMode: 0o660}
	if _, err := listen(cfg); err == nil {
		t.Fatal("want an error when the socket directory cannot be created")
	}
}

func TestVersionSubcommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"-version"}, {"--version"}} {
		var stdout, stderr strings.Builder

		if err := run(args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}

		if !strings.HasPrefix(stdout.String(), "gantry-snapshotter ") {
			t.Errorf("run(%v) printed %q", args, stdout.String())
		}
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	var stdout, stderr strings.Builder

	if err := run([]string{"-log-format", "yaml"}, &stdout, &stderr); err == nil {
		t.Fatal("want an error")
	}
}
