// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestParseSize covers every accepted suffix and the error paths.
func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"0", 0, false},
		{"1B", 1, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"10MB", 10_000_000, false},
		{"10MiB", 10 * 1024 * 1024, false},
		{"1GB", 1_000_000_000, false},
		{"1GiB", 1024 * 1024 * 1024, false},
		{"1TB", 1_000_000_000_000, false},
		{"1TiB", 1024 * 1024 * 1024 * 1024, false},
		{"1.5GB", 1_500_000_000, false},
		{"  10MiB  ", 10 * 1024 * 1024, false},
		{"10mib", 10 * 1024 * 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1XB", 0, true},
		{"-5MB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSize(%q) = %d, want error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Errorf("parseSize(%q) unexpected err: %v", tt.in, err)
				return
			}

			if got != tt.want {
				t.Errorf("parseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestFormatSize spot-checks the human-readable rendering at the
// boundaries between units.
func TestFormatSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KiB"},
		{2048, "2.00 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{10 * 1024 * 1024, "10.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
	}

	for _, tt := range tests {
		got := formatSize(tt.in)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestGenerate_SeededDeterministic_Concurrent verifies that two
// generate runs with the same --seed produce byte-identical bodies
// even under concurrency > 1. The previous implementation used a
// shared math/rand source serialised through a mutex; bytes flowed
// to whichever goroutine acquired the lock first, so the same
// invocation could produce different per-blob bytes between runs
// based on goroutine-scheduling order. The fixed implementation
// derives each blob's stream from (seed + blobIndex), so each blob
// is a pure function of its index and seed regardless of
// completion ordering.
//
// Regression for C-6.
func TestGenerate_SeededDeterministic_Concurrent(t *testing.T) {
	t.Parallel()

	bodiesA := startFakeAzurite(t)
	defer bodiesA.close()

	bodiesB := startFakeAzurite(t)
	defer bodiesB.close()

	g := defaultGlobalFlags()
	g.endpoint = bodiesA.url
	g.account = "devstoreaccount1"
	g.accountKey = base64.StdEncoding.EncodeToString([]byte("test-shared-key-placeholder--32b"))
	g.containerName = "ctr"

	o := &generateOpts{
		sizeStr:     "4KiB",
		count:       4,
		prefix:      "synth-",
		seed:        42,
		concurrency: 4, // deliberate: prove determinism survives parallel uploads
	}

	if err := runGenerate(context.Background(), g, o); err != nil {
		t.Fatalf("first runGenerate: %v", err)
	}

	g.endpoint = bodiesB.url

	if err := runGenerate(context.Background(), g, o); err != nil {
		t.Fatalf("second runGenerate: %v", err)
	}

	for _, name := range []string{"synth-0", "synth-1", "synth-2", "synth-3"} {
		a := bodiesA.get(name)
		b := bodiesB.get(name)

		if len(a) == 0 {
			t.Errorf("blob %q missing from first run", name)
			continue
		}

		if len(a) != len(b) {
			t.Errorf("blob %q length differs across runs: %d vs %d", name, len(a), len(b))
			continue
		}

		if string(a) != string(b) {
			t.Errorf("blob %q bytes differ across two seeded runs (concurrency=%d)",
				name, o.concurrency)
		}
	}
}

// TestGenerate_SeededDifferentBlobsHaveDifferentContent verifies the
// per-blob seeding produces distinct streams (so two blobs in the
// same run are not byte-identical).
func TestGenerate_SeededDifferentBlobsHaveDifferentContent(t *testing.T) {
	t.Parallel()

	bodies := startFakeAzurite(t)
	defer bodies.close()

	g := defaultGlobalFlags()
	g.endpoint = bodies.url
	g.account = "devstoreaccount1"
	g.accountKey = base64.StdEncoding.EncodeToString([]byte("test-shared-key-placeholder--32b"))
	g.containerName = "ctr"

	o := &generateOpts{
		sizeStr:     "4KiB",
		count:       2,
		prefix:      "synth-",
		seed:        99,
		concurrency: 2,
	}

	if err := runGenerate(context.Background(), g, o); err != nil {
		t.Fatalf("runGenerate: %v", err)
	}

	a := bodies.get("synth-0")
	b := bodies.get("synth-1")

	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("blobs missing: synth-0=%d synth-1=%d", len(a), len(b))
	}

	if string(a) == string(b) {
		t.Errorf("synth-0 and synth-1 have identical content; per-blob seeding broken")
	}
}

// fakeAzurite is a minimal httptest-backed server that:
//   - accepts container Create (PUT ?restype=container) with 201;
//   - accepts block-blob PUT at /<account>/<container>/<blob> with 201;
//   - records received bodies indexed by blob name;
//   - rejects everything else with 400 so test failures are loud.
type fakeAzurite struct {
	srv      *httptest.Server
	url      string
	mu       atomic.Pointer[map[string][]byte]
	requests atomic.Int64
}

func startFakeAzurite(t *testing.T) *fakeAzurite {
	t.Helper()

	f := &fakeAzurite{}
	bodies := make(map[string][]byte)
	f.mu.Store(&bodies)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		// path: /<account>/<container>[/<blob>]
		// We don't validate the SAS / shared-key signature; the SDK
		// signs every request and we trust the format.
		path := strings.TrimPrefix(r.URL.Path, "/")

		parts := strings.SplitN(path, "/", 3)
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		// Container create: PUT /<account>/<container>?restype=container
		if r.Method == http.MethodPut && len(parts) == 2 && r.URL.Query().Get("restype") == "container" {
			w.WriteHeader(http.StatusCreated)
			return
		}

		if r.Method == http.MethodPut && len(parts) == 3 {
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // best-effort test reader
			_ = r.Body.Close()            //nolint:errcheck // best-effort

			cur := *f.mu.Load()
			next := make(map[string][]byte, len(cur)+1)

			for k, v := range cur {
				next[k] = v
			}

			next[parts[2]] = body
			f.mu.Store(&next)

			w.Header().Set("ETag", "\"fake-etag\"")
			w.Header().Set("Last-Modified", "Thu, 01 Jan 1970 00:00:00 GMT")
			w.WriteHeader(http.StatusCreated)

			return
		}

		http.Error(w, "unexpected request: "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	})

	f.srv = httptest.NewServer(mux)
	// Account-suffixed endpoint shape the SDK expects.
	f.url = f.srv.URL + "/devstoreaccount1/"

	// Validate the URL parses cleanly.
	if _, err := url.Parse(f.url); err != nil {
		t.Fatalf("fake azurite endpoint parse: %v", err)
	}

	return f
}

func (f *fakeAzurite) close() {
	f.srv.Close()
}

func (f *fakeAzurite) get(name string) []byte {
	cur := *f.mu.Load()
	return cur[name]
}
