// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racerblock/ublk"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseOptionsDefaults(t *testing.T) {
	var out bytes.Buffer

	o, err := parseOptions([]string{"--cache", "blobs", "--key", testKey}, &out)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}

	if o.cache != "blobs" || o.key.String() != testKey {
		t.Fatalf("cache/key = %q/%q", o.cache, o.key.String())
	}

	if o.id != -1 || o.queues != 0 || o.depth != 0 || o.etag != "" {
		t.Fatalf("unexpected defaults: %+v", o)
	}

	if want := defaultConnections(0, 0); o.maxConnections != want {
		t.Fatalf("maxConnections = %d, want %d", o.maxConnections, want)
	}

	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestParseOptionsAllFlags(t *testing.T) {
	o, err := parseOptions([]string{
		"--cache", "blobs", "--key", testKey, "--etag", `"v1"`, "--id", "7",
		"--queues", "2", "--depth", "64", "--max-io-bytes", "524288",
		"--chunk-size", "1048576", "--cache-bytes", "33554432", "--readahead-max", "-1",
		"--warm-ttl", "10m", "--max-connections", "32", "--log-level", "debug",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}

	if o.etag != `"v1"` || o.id != 7 || o.queues != 2 || o.depth != 64 || o.maxIOBytes != 524288 ||
		o.chunkSize != 1<<20 || o.cacheBytes != 32<<20 || o.readaheadMax != -1 ||
		o.warmTTL != 10*time.Minute || o.maxConnections != 32 || o.logLevel.String() != "DEBUG" {
		t.Fatalf("unexpected options: %+v", o)
	}
}

func TestParseOptionsErrors(t *testing.T) {
	base := []string{"--cache", "blobs", "--key", testKey}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing cache", []string{"--key", testKey}, "--cache is required"},
		{"bad cache", []string{"--cache", "Bad_Name", "--key", testKey}, "invalid --cache"},
		{"missing key", []string{"--cache", "blobs"}, "--key is required"},
		{"bad key", []string{"--cache", "blobs", "--key", "abc"}, "invalid --key"},
		{"upper key", []string{"--cache", "blobs", "--key", strings.ToUpper(testKey)}, "invalid --key"},
		{"bad id", append(base, "--id", "-2"), "--id"},
		{"id above max", append(base, "--id", strconv.Itoa(ublk.MaxDeviceID+1)), "--id"},
		{"id 2^32", append(base, "--id", "4294967296"), "id"},
		{"queues", append(base, "--queues", "65"), "--queues"},
		{"negative queues", append(base, "--queues", "-1"), "--queues"},
		{"depth", append(base, "--depth", "4097"), "--depth"},
		{"max io", append(base, "--max-io-bytes", "-1"), "--max-io-bytes"},
		{"chunk", append(base, "--chunk-size", "-1"), "--chunk-size"},
		{"cache bytes", append(base, "--cache-bytes", "-1"), "--cache-bytes"},
		{"warm ttl", append(base, "--warm-ttl", "-1s"), "--warm-ttl"},
		{"connections", append(base, "--max-connections", "-1"), "--max-connections"},
		{"log level", append(base, "--log-level", "loud"), "--log-level"},
		{"unknown flag", append(base, "--nope"), "invalid flags"},
		{"positional", append(base, "extra"), "positional"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseOptions(tt.args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseOptionsIDBounds(t *testing.T) {
	for _, id := range []int{-1, 0, ublk.MaxDeviceID} {
		o, err := parseOptions([]string{"--cache", "blobs", "--key", testKey, "--id", strconv.Itoa(id)}, &bytes.Buffer{})
		if err != nil || o.id != id {
			t.Fatalf("--id %d: id = %d, err = %v", id, o.id, err)
		}
	}
}

func TestParseOptionsHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		var out bytes.Buffer

		_, err := parseOptions(args, &out)
		if !errors.Is(err, errHelp) {
			t.Fatalf("%v: err = %v, want errHelp", args, err)
		}

		if out.Len() == 0 {
			t.Fatalf("%v: no output", args)
		}
	}

	var out bytes.Buffer

	_, _ = parseOptions([]string{"--help"}, &out)
	if !strings.Contains(out.String(), "--key") && !strings.Contains(out.String(), "-key") {
		t.Fatalf("help does not list flags: %q", out.String())
	}
}

func TestRunReturnsParseErrors(t *testing.T) {
	err := run(context.Background(), []string{"--cache", "blobs"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--key is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsMissingHeaderFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	err := run(context.Background(), []string{"--cache", "blobs", "--key", testKey, "--authorization-file", missing},
		&bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--authorization-file") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultConnections(t *testing.T) {
	if got, want := defaultConnections(0, 0), min(ublk.DefaultQueues()*ublk.DefaultDepth, maxConnections); got != want {
		t.Fatalf("defaultConnections(0, 0) = %d, want %d", got, want)
	}

	if got := defaultConnections(2, 8); got != 16 {
		t.Fatalf("defaultConnections(2, 8) = %d, want 16", got)
	}

	if got := defaultConnections(64, 4096); got != maxConnections {
		t.Fatalf("defaultConnections(64, 4096) = %d, want %d", got, maxConnections)
	}
}

func TestReadHeaderFile(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		t.Helper()

		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	if got, err := readHeaderFile("", "--x"); err != nil || got != "" {
		t.Fatalf("empty path = %q, %v", got, err)
	}

	if got, err := readHeaderFile(write("crlf", "Bearer abc\r\n"), "--x"); err != nil || got != "Bearer abc" {
		t.Fatalf("crlf = %q, %v", got, err)
	}

	if got, err := readHeaderFile(write("plain", "bucket=a"), "--x"); err != nil || got != "bucket=a" {
		t.Fatalf("plain = %q, %v", got, err)
	}

	exact := strings.Repeat("a", maxHeaderFileBytes)
	if got, err := readHeaderFile(write("exact", exact+"\n"), "--x"); err != nil || got != exact {
		t.Fatalf("exact limit: len %d, %v", len(got), err)
	}

	if _, err := readHeaderFile(write("long", exact+"a"), "--x"); err == nil {
		t.Fatal("oversized value accepted")
	}

	for name, content := range map[string]string{
		"exact-plain": exact,
		"exact-crlf":  exact + "\r\n",
	} {
		if got, err := readHeaderFile(write(name, content), "--x"); err != nil || got != exact {
			t.Fatalf("%s: len %d, %v", name, len(got), err)
		}
	}

	for name, content := range map[string]string{
		"crlf-extra":   exact + "\r\nx",
		"crlf-lf":      exact + "\r\n\n",
		"crlf-cr":      exact + "\r\n\r",
		"crlf-crlf":    exact + "\r\n\r\n",
		"lf-lf":        exact + "\n\n",
		"cr-cr":        exact + "\r\r",
		"crlf-extra-2": exact + "\r\nxyz",
	} {
		for _, flag := range []string{"--authorization-file", "--metadata-file"} {
			_, err := readHeaderFile(write(name, content), flag)
			if err == nil || !strings.HasPrefix(err.Error(), flag+":") {
				t.Fatalf("%s %s: err = %v", name, flag, err)
			}
		}
	}
}
