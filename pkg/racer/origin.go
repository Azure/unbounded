// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Store supplies origin objects. Methods must be safe for concurrent calls and
// honor ctx. Stat must avoid reading payloads. Open must atomically acquire an
// immutable snapshot matching the ETag from Stat, or return ErrVersionChanged if
// that version is unavailable. ETags must be quoted 64-character lowercase hex
// representation IDs. A content checksum is suitable, as is a domain-separated
// hash of a permanently immutable name. Never reuse an ID for changed bytes at
// the same target, including after deletion and recreation.
// Use fs.ErrNotExist and fs.ErrPermission for HTTP 404 and 403;
// ErrVersionChanged produces 412, and other errors produce 500.
type Store interface {
	Stat(ctx context.Context, target string) (Metadata, error)
	Open(ctx context.Context, target, etag string) (Source, error)
}

// Source pins one representation for the lifetime of a GET request and exposes
// exactly the size reported by Stat for that version. Close is called exactly
// once after a successful Open, including disconnected clients. The Store retains
// ownership of a source returned together with an error. Open must not return a
// nil Source on success.
type Source interface {
	io.ReaderAt
	io.Closer
}

// Origin implements the same object HEAD/GET contract as a Racer cache listener.
// Serve it directly with http.Server.Serve on a filesystem Unix listener to
// preserve raw targets without path cleaning.
// Configure server timeouts and admission limits for your deployment.
type Origin struct{ store Store }

// NewOrigin binds the object-read protocol to a concurrent storage adapter.
func NewOrigin(store Store) (*Origin, error) {
	if store == nil {
		return nil, fmt.Errorf("racer: nil origin store")
	}

	return &Origin{store: store}, nil
}

func (o *Origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, HEAD")
		emptyResponse(w, http.StatusMethodNotAllowed)

		return
	}

	target := r.RequestURI
	// Absolute-form requests select the same raw path/query as origin-form.
	// Strip only scheme/authority; parsing and rebuilding can normalize the target.
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			target = rest[i:]
			if target[0] == '?' {
				target = "/" + target
			}
		} else {
			target = "/"
		}
	}

	if !validTarget(target) || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		emptyResponse(w, http.StatusBadRequest)
		return
	}

	m, err := o.store.Stat(r.Context(), target)
	if err != nil {
		storeError(w, err)
		return
	}

	if !validMetadata(m) {
		emptyResponse(w, http.StatusInternalServerError)
		return
	}

	if !originConditions(w, r, m) {
		return
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
		w.WriteHeader(http.StatusOK)

		return
	}

	start, length, status := objectRange(r.Header, m)
	if status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", m.Size))
		emptyResponse(w, http.StatusRequestedRangeNotSatisfiable)

		return
	}

	source, err := o.store.Open(r.Context(), target, m.ETag)
	if err != nil {
		storeError(w, err)
		return
	}

	if source == nil {
		emptyResponse(w, http.StatusInternalServerError)
		return
	}
	defer source.Close() //nolint:errcheck // Release the snapshot after the response has been sent or aborted.

	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", contentRange(start, start+length-1, m.Size))
	}

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(status)

	buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
	defer copyBuffers.Put(buf)

	n, err := io.CopyBuffer(w, io.NewSectionReader(contextReaderAt{r.Context(), source}, start, length), *buf)
	if err != nil || n != length {
		// Abort instead of letting net/http finish a successful but short response.
		panic(http.ErrAbortHandler)
	}
}

type contextReaderAt struct {
	ctx context.Context
	r   io.ReaderAt
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	return r.r.ReadAt(p, off)
}

func validMetadata(m Metadata) bool {
	return m.Size >= 0 && checksumETag(m.ETag) && (m.TTL == nil || *m.TTL >= 0)
}

func metadataHeaders(h http.Header, m Metadata) {
	h.Set("Accept-Ranges", "bytes")

	if m.ETag != "" {
		h.Set("ETag", m.ETag)
	}

	if m.TTL != nil {
		h.Set("Cache-Control", "max-age="+strconv.FormatInt(int64(*m.TTL/time.Second), 10))
	}
}

func emptyResponse(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		emptyResponse(w, http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		emptyResponse(w, http.StatusForbidden)
	case errors.Is(err, ErrVersionChanged):
		emptyResponse(w, http.StatusPreconditionFailed)
	default:
		emptyResponse(w, http.StatusInternalServerError)
	}
}

// Both conditional fields are validated before evaluating them in protocol order.
func originConditions(w http.ResponseWriter, r *http.Request, m Metadata) bool {
	match, matchOK := matchesETag(r.Header.Values("If-Match"), m.ETag, true)

	none, noneOK := matchesETag(r.Header.Values("If-None-Match"), m.ETag, false)
	if !matchOK || !noneOK {
		emptyResponse(w, http.StatusBadRequest)
		return false
	}

	metadataHeaders(w.Header(), m)

	if len(r.Header.Values("If-Match")) != 0 && !match {
		emptyResponse(w, http.StatusPreconditionFailed)
		return false
	}

	if none {
		// net/http omits Content-Length on 304; no payload is sent.
		w.WriteHeader(http.StatusNotModified)
		return false
	}

	return true
}

func objectRange(h http.Header, m Metadata) (start, length int64, status int) {
	full := func() (int64, int64, int) { return 0, m.Size, http.StatusOK }
	if values := h.Values("If-Range"); len(values) != 0 && (len(values) != 1 || !strongETag(m.ETag) || strings.Trim(values[0], " \t") != m.ETag) {
		return full()
	}

	values := h.Values("Range")
	if len(values) != 1 {
		return full()
	}

	unit, spec, ok := strings.Cut(strings.Trim(values[0], " \t"), "=")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return full()
	}

	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return full()
	}
	// Rust parses unsigned 64-bit endpoints, clipping before conversion to size.
	parse := func(s string) (uint64, bool) {
		if s == "" {
			return 0, false
		}

		for i := range s {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
		}

		n, err := strconv.ParseUint(s, 10, 64)

		return n, err == nil
	}
	size := uint64(m.Size)

	if a == "" {
		n, valid := parse(b)
		if !valid {
			return full()
		}

		if n == 0 || size == 0 {
			return 0, 0, http.StatusRequestedRangeNotSatisfiable
		}

		length = int64(min(n, size))

		return m.Size - length, length, http.StatusPartialContent
	}

	first, valid := parse(a)
	if !valid {
		return full()
	}

	end := size

	if b != "" {
		last, valid := parse(b)
		if !valid || last < first {
			return full()
		}

		if last < size {
			end = last + 1
		}
	}

	if first >= size {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}

	return int64(first), int64(end - first), http.StatusPartialContent
}

// Parse lists rather than splitting on commas: commas are legal inside an ETag.
// As in Racer, tolerate empty list members, but not an entirely empty list.
func matchesETag(values []string, tag string, strong bool) (match, valid bool) {
	members := 0

	for _, field := range values {
		value := strings.Trim(field, " \t")
		if value == "*" {
			return true, len(values) == 1
		}

		for value != "" {
			if value[0] == ',' {
				value = strings.Trim(value[1:], " \t")
				continue
			}

			weak := strings.HasPrefix(value, "W/")

			candidate := value
			if weak {
				candidate = candidate[2:]
			}

			if !strings.HasPrefix(candidate, "\"") {
				return false, false
			}

			end := strings.IndexByte(candidate[1:], '"')
			if end < 0 {
				return false, false
			}

			candidate = candidate[:end+2]
			if !validETag(candidate) {
				return false, false
			}

			members++
			match = match || (strong && !weak && candidate == tag) || (!strong && candidate == strings.TrimPrefix(tag, "W/"))

			consumed := len(candidate)
			if weak {
				consumed += 2
			}

			value = strings.Trim(value[consumed:], " \t")
			if value == "" {
				break
			}

			if value[0] != ',' {
				return false, false
			}

			value = strings.Trim(value[1:], " \t")
		}
	}

	return match, len(values) == 0 || members > 0
}
