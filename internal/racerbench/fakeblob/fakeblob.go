// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fakeblob is a minimal, low-CPU fake of the Azure Blob Get Blob
// Properties (HEAD) and Get Blob (GET) operations, used only by local origin
// benchmarks. Content is deterministic per key so readers can verify it
// without storing objects.
package fakeblob

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// patternSize is prime so per-key offsets do not align with pages.
const patternSize = 1<<20 + 7

// APIVersion is reported in x-ms-version.
const APIVersion = "2023-11-03"

var pattern = func() []byte {
	b := make([]byte, patternSize)
	x := uint32(2463534242)

	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}

	return b
}()

func seed(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))

	return int64(h.Sum64() % patternSize)
}

// Fill writes the content of key starting at offset into p.
func Fill(key string, offset int64, p []byte) {
	at := (offset + seed(key)) % patternSize
	for len(p) > 0 {
		n := copy(p, pattern[at:])
		p = p[n:]
		at = 0
	}
}

// Verifier checks a stream against the content of one key.
type Verifier struct {
	key      string
	at       int64
	offset   int64
	mismatch bool
}

// NewVerifier returns a writer that fails on the first mismatching byte.
func NewVerifier(key string) *Verifier {
	return &Verifier{key: key, at: seed(key)}
}

// Write compares p to the expected content without copying.
func (v *Verifier) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		want := pattern[v.at:]
		n := min(len(want), len(p))

		if string(p[:n]) != string(want[:n]) {
			v.mismatch = true

			return 0, fmt.Errorf("fakeblob: %q content mismatch near offset %d", v.key, v.offset)
		}

		p = p[n:]
		v.offset += int64(n)
		v.at = (v.at + int64(n)) % patternSize
	}

	return total, nil
}

// Mismatch reports whether verification failed.
func (v *Verifier) Mismatch() bool { return v.mismatch }

// Bytes is how many bytes have been verified.
func (v *Verifier) Bytes() int64 { return v.offset }

// Stats counts served requests.
type Stats struct {
	Heads, Gets, Errors, Bytes atomic.Int64
}

// Snapshot is a JSON view of Stats.
type Snapshot struct {
	Heads  int64 `json:"heads"`
	Gets   int64 `json:"gets"`
	Errors int64 `json:"errors"`
	Bytes  int64 `json:"bytes"`
}

// Server serves /<account>/<container>/<key> for any account and container.
// Every object has Size bytes unless the key starts with "size-<n>/".
type Server struct {
	Size     int64
	Modified time.Time
	Stats    Stats
}

// StatsPath returns counters as JSON.
const StatsPath = "/_fake/stats"

func (s *Server) snapshot() Snapshot {
	return Snapshot{Heads: s.Stats.Heads.Load(), Gets: s.Stats.Gets.Load(), Errors: s.Stats.Errors.Load(), Bytes: s.Stats.Bytes.Load()}
}

// SizeOf returns the object size for key.
func (s *Server) SizeOf(key string) int64 {
	if rest, ok := strings.CutPrefix(key, "size-"); ok {
		if n, _, ok := strings.Cut(rest, "/"); ok {
			if v, err := strconv.ParseInt(n, 10, 64); err == nil && v >= 0 {
				return v
			}
		}
	}

	return s.Size
}

// ETag returns the strong quoted ETag of key.
func (s *Server) ETag(key string) string {
	return fmt.Sprintf("\"0x8D%012X%04X\"", seed(key), s.SizeOf(key)&0xffff)
}

func (s *Server) fail(w http.ResponseWriter, status int, code string) {
	s.Stats.Errors.Add(1)
	w.Header().Set("x-ms-error-code", code)
	w.Header().Set("x-ms-version", APIVersion)
	w.WriteHeader(status)
}

// parseRange parses "bytes=a-b" or "bytes=a-". ok is false for absent ranges.
func parseRange(value string) (first, last int64, ok bool, err error) {
	if value == "" {
		return 0, -1, false, nil
	}

	spec, found := strings.CutPrefix(value, "bytes=")
	a, b, dash := strings.Cut(spec, "-")

	if !found || !dash || strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("bad range %q", value)
	}

	if first, err = strconv.ParseInt(a, 10, 64); err != nil || first < 0 {
		return 0, 0, false, fmt.Errorf("bad range %q", value)
	}

	last = -1
	if b != "" {
		if last, err = strconv.ParseInt(b, 10, 64); err != nil || last < first {
			return 0, 0, false, fmt.Errorf("bad range %q", value)
		}
	}

	return first, last, true, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == StatsPath {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(s.snapshot()); err != nil {
			s.Stats.Errors.Add(1)
		}

		return
	}

	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		s.fail(w, http.StatusNotFound, "ResourceNotFound")
		return
	}

	key := parts[2]
	size, tag := s.SizeOf(key), s.ETag(key)

	if m := r.Header.Get("If-Match"); m != "" && m != "*" && m != tag {
		s.fail(w, http.StatusPreconditionFailed, "ConditionNotMet")
		return
	}

	h := w.Header()
	h.Set("ETag", tag)
	h.Set("Last-Modified", s.Modified.UTC().Format(http.TimeFormat))
	h.Set("Content-Type", "application/octet-stream")
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("x-ms-version", APIVersion)
	h.Set("Accept-Ranges", "bytes")

	if v := r.URL.Query().Get("versionid"); v != "" {
		h.Set("x-ms-version-id", v)
	}

	switch r.Method {
	case http.MethodHead:
		s.Stats.Heads.Add(1)
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)

		return
	case http.MethodGet:
		s.Stats.Gets.Add(1)
	default:
		s.fail(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
		return
	}

	rangeHeader := r.Header.Get("x-ms-range")
	if rangeHeader == "" {
		rangeHeader = r.Header.Get("Range")
	}

	first, last, ranged, err := parseRange(rangeHeader)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "InvalidHeaderValue")
		return
	}

	status := http.StatusOK

	if ranged {
		if first >= size {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			s.fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")

			return
		}

		if last < 0 || last >= size {
			last = size - 1
		}

		status = http.StatusPartialContent

		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
	} else {
		first, last = 0, size-1
	}

	h.Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	w.WriteHeader(status)

	at := (first + seed(key)) % patternSize
	for remaining := last - first + 1; remaining > 0; {
		n := min(remaining, int64(patternSize)-at)

		written, err := w.Write(pattern[at : at+n])
		s.Stats.Bytes.Add(int64(written))

		if err != nil {
			return
		}

		remaining -= n
		at = 0
	}
}
