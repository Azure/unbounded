// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunRoundtripWith_OutputFormat captures stderr from a successful
// roundtrip and verifies the human-facing presentation contract that
// the README and the developer workflow depend on:
//
//   - The source SHA-256 is printed on its own indented line under
//     the "source:" heading.
//   - Each iter prints status / bytes / elapsed / rate, then the
//     received SHA-256 on its own indented line, followed by a
//     MATCH marker (or MISMATCH on the failure path).
//   - The PASS line carries the short-form sha256 and the iteration
//     count.
//
// Drift here is a real user-visible regression, so we lock the
// contract in tests rather than only documenting it in comments.
func TestRunRoundtripWith_OutputFormat(t *testing.T) {
	// Not t.Parallel: swaps os.Stderr.

	const (
		bucket = "test-bucket"
		key    = "test-key"
		body   = "hello world"
		etag   = "real-etag"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+etag+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	oc := newFakeOriginClient("fake", bucket)
	oc.put(key, etag, []byte(body))

	edge := newEdgeClient(server.URL, 2*time.Second)

	// "hello world" sha256 is well-known; using the literal here
	// makes the test self-documenting.
	const wantSHA = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

	t.Run("single iter", func(t *testing.T) {
		o := &roundtripOpts{key: key, repeat: 1}

		out := captureStderr(t, func() {
			if err := runRoundtripWith(context.Background(), oc, edge, o); err != nil {
				t.Fatalf("runRoundtripWith: %v", err)
			}
		})

		assertContainsLines(t, out,
			"source: "+key+" (11 B)",
			"  sha256: "+wantSHA,
			"  sha256: "+wantSHA+"  MATCH",
			"PASS sha256="+wantSHA[:8]+"... (1 iter)",
		)
	})

	t.Run("multi iter pluralization", func(t *testing.T) {
		o := &roundtripOpts{key: key, repeat: 3}

		out := captureStderr(t, func() {
			if err := runRoundtripWith(context.Background(), oc, edge, o); err != nil {
				t.Fatalf("runRoundtripWith: %v", err)
			}
		})

		// One MATCH per iteration.
		if got := strings.Count(out, "MATCH"); got != 3 {
			t.Errorf("MATCH count = %d, want 3\n%s", got, out)
		}

		if !strings.Contains(out, "PASS sha256="+wantSHA[:8]+"... (3 iters)") {
			t.Errorf("PASS line missing or wrong plural form:\n%s", out)
		}
	})
}

// TestRunRoundtripWith_MismatchMarker verifies the per-iter
// MISMATCH marker fires when the received hash differs from the
// source hash, and that the existing copy-pasteable MISMATCH summary
// block still prints below it.
func TestRunRoundtripWith_MismatchMarker(t *testing.T) {
	// Not t.Parallel: swaps os.Stderr.

	const (
		bucket   = "test-bucket"
		key      = "test-key"
		realBody = "hello world"
		// edge returns a DIFFERENT body, so received hash != source hash.
		edgeBody = "hello WORLD"
		etag     = "real-etag"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+etag+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(edgeBody)))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(edgeBody))
	}))
	t.Cleanup(server.Close)

	oc := newFakeOriginClient("fake", bucket)
	oc.put(key, etag, []byte(realBody))

	edge := newEdgeClient(server.URL, 2*time.Second)

	o := &roundtripOpts{key: key, repeat: 1}

	out := captureStderr(t, func() {
		err := runRoundtripWith(context.Background(), oc, edge, o)
		if err == nil {
			t.Fatal("expected error from runRoundtripWith on mismatch")
		}

		if !strings.Contains(err.Error(), "checksum mismatch on iter 0") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Per-iter marker.
	if !strings.Contains(out, "  MISMATCH\n") {
		t.Errorf("expected per-iter MISMATCH marker in output:\n%s", out)
	}

	// Copy-paste summary block still present.
	for _, want := range []string{
		"MISMATCH on iter 0",
		"  source sha256:",
		"  received sha256:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// TestShortHash covers the eight-char-plus-ellipsis truncation used
// by the PASS line.
func TestShortHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", "b94d27b9..."},
		{"12345678", "12345678..."},
		{"short", "short..."},
		{"", "..."},
	}

	for _, tt := range tests {
		got := shortHash(tt.in)
		if got != tt.want {
			t.Errorf("shortHash(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestIterLabel covers singular vs plural for the PASS line.
func TestIterLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int
		want string
	}{
		{1, "1 iter"},
		{2, "2 iters"},
		{0, "0 iters"},
		{100, "100 iters"},
	}

	for _, tt := range tests {
		if got := iterLabel(tt.in); got != tt.want {
			t.Errorf("iterLabel(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// captureStderr swaps os.Stderr for the duration of fn and returns
// everything fn wrote to it. Safe to call sequentially across
// subtests; NOT safe to use with t.Parallel because os.Stderr is
// a process-global.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stderr

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	os.Stderr = w

	done := make(chan []byte, 1)

	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	fn()

	_ = w.Close()
	os.Stderr = orig

	return string(<-done)
}

// assertContainsLines asserts that every want string appears as a
// substring of out, reporting all failures in one call so a broken
// output format surfaces every mismatch at once.
func assertContainsLines(t *testing.T, out string, wants ...string) {
	t.Helper()

	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("expected %q in stderr; got:\n%s", w, out)
		}
	}
}
