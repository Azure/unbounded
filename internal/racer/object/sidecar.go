// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// SidecarConfig selects the object namespace and optional bucket allowlist.
type SidecarConfig struct {
	Namespace string
	Buckets   []string
}

type sidecar struct {
	client *racersdk.Client
	scope  scope
}

// NewSidecar serves a read-only, trusted pod endpoint. The caller owns the client
// and must bind the server to loopback. SigV4 headers are ignored, not verified or
// forwarded. Presigned query parameters and unsupported S3 operations are rejected.
// Only known auth/plumbing x-amz headers are accepted. Owner checks, SSE-C,
// requester-pays, and checksum requests are unsupported and return NotImplemented.
// Install this handler directly: a path-cleaning mux changes object identities.
func NewSidecar(client *racersdk.Client, config SidecarConfig) (http.Handler, error) {
	if client == nil {
		return nil, errors.New("racer-object: missing sidecar client")
	}

	bound, err := newScope(config.Namespace, config.Buckets)
	if errors.Is(err, errInvalidNamespace) {
		return nil, errors.New("racer-object: invalid sidecar namespace")
	}

	if err != nil {
		return nil, errors.New("racer-object: invalid sidecar bucket")
	}

	return &sidecar{client: client, scope: bound}, nil
}

func (s *sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if failure := s.serve(w, r); failure != 0 {
		failure.write(w, r)
	}
}

// serve returns failures only before headers are committed, so XML cannot be
// appended to an object body. A failed committed stream must abort instead.
func (s *sidecar) serve(w http.ResponseWriter, r *http.Request) sidecarFailure {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		return http.StatusMethodNotAllowed
	}

	request, version, failure := s.request(r)
	if failure != 0 {
		return failure
	}

	match, ok := sidecarCondition(r.Header, "If-Match")
	if !ok {
		return http.StatusBadRequest
	}

	none, ok := sidecarCondition(r.Header, "If-None-Match")
	if !ok {
		return http.StatusBadRequest
	}

	m, err := s.client.Stat(r.Context(), request)
	if err != nil {
		return sidecarSDKFailure(err)
	}

	if match.present && !match.matches(m.ETag, false) {
		return http.StatusPreconditionFailed
	}

	if none.present && none.matches(m.ETag, true) {
		w.Header().Set("ETag", m.ETag)
		w.WriteHeader(http.StatusNotModified)

		return 0
	}

	offset, length, partial, status := sidecarRange(r.Header, uint64(m.Size))
	if status != 0 {
		if status == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", m.Size))
		}

		return sidecarFailure(status)
	}

	read := sidecarRead{m, version, offset, length, partial}
	if r.Method == http.MethodHead {
		read.writeHeaders(w)
		return 0
	}

	return s.stream(w, r, request, read)
}

func (s *sidecar) stream(w http.ResponseWriter, r *http.Request, request racersdk.Request, read sidecarRead) sidecarFailure {
	v, err := s.client.Get(r.Context(), request, racersdk.ReadOptions{
		ETag: read.metadata.ETag, Offset: int64(read.offset), Length: int64(read.length),
	})
	if err != nil {
		return sidecarSDKFailure(err)
	}

	defer func() { _ = v.Close() }() //nolint:errcheck // Close releases admission; its error cannot change the response.

	// An ETag pin must not change the headers selected by Stat.
	if m := v.Metadata(); m.Size != read.metadata.Size || m.ContentType != read.metadata.ContentType {
		return http.StatusBadGateway
	}

	return read.writeBody(w, v)
}

func (read sidecarRead) writeBody(w http.ResponseWriter, v *racersdk.Object) sidecarFailure {
	// Empty success is not committed until the SDK has validated Complete.
	if read.length == 0 {
		if _, err := v.WriteTo(w); err != nil {
			return sidecarSDKFailure(err)
		}
	}

	read.writeHeaders(w)

	if read.length != 0 {
		if _, err := v.WriteTo(w); err != nil {
			panic(http.ErrAbortHandler)
		}
	}

	return 0
}

func (s *sidecar) request(r *http.Request) (racersdk.Request, string, sidecarFailure) {
	if !sidecarSupportedHeaders(r.Header) {
		return racersdk.Request{}, "", http.StatusNotImplemented
	}
	// Split before decoding so an escaped slash is key data, not a bucket boundary.
	// Decode each part exactly once; never clean dot segments or repeated slashes.
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), "/")

	bucket, key, found := strings.Cut(path, "/")
	if !ok || !found || bucket == "" || key == "" {
		return racersdk.Request{}, "", http.StatusBadRequest
	}

	// EscapedPath always supplies valid escapes, including when RawPath is invalid.
	bucket, _ = url.PathUnescape(bucket) //nolint:errcheck // EscapedPath guarantees valid escapes.
	key, _ = url.PathUnescape(key)       //nolint:errcheck // EscapedPath guarantees valid escapes.

	if !s.scope.allows(bucket) {
		return racersdk.Request{}, "", http.StatusForbidden
	}

	version, failure := sidecarVersion(r)
	if failure != 0 {
		return racersdk.Request{}, "", failure
	}

	request, err := NewRequest(s.scope.namespace, bucket, key, version)
	if err != nil {
		return racersdk.Request{}, "", http.StatusBadRequest
	}

	return request, version, 0
}

// sidecarVersion rejects query options that would silently change S3 semantics.
func sidecarVersion(r *http.Request) (string, sidecarFailure) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", http.StatusBadRequest
	}

	for name, values := range query {
		if len(values) != 1 || values[0] == "" {
			return "", http.StatusBadRequest
		}

		switch name {
		case "versionId":
		case "x-id":
			want := "GetObject"
			if r.Method == http.MethodHead {
				want = "HeadObject"
			}

			if values[0] != want {
				return "", http.StatusNotImplemented
			}
		default:
			return "", http.StatusNotImplemented
		}
	}

	version := query.Get("versionId")
	for _, c := range []byte(version) {
		if c < 0x20 || c == 0x7f {
			return "", http.StatusBadRequest
		}
	}

	return version, 0
}

// sidecarRead keeps the selected metadata and range together for HEAD and GET.
type sidecarRead struct {
	metadata       racersdk.Metadata
	version        string
	offset, length uint64
	partial        bool
}

func (read sidecarRead) writeHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("ETag", read.metadata.ETag)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatUint(read.length, 10))

	contentType := read.metadata.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	h.Set("Content-Type", contentType)

	if read.version != "" {
		h.Set("x-amz-version-id", read.version)
	}

	status := http.StatusOK
	if read.partial {
		status = http.StatusPartialContent

		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", read.offset, read.offset+read.length-1, read.metadata.Size))
	}

	w.WriteHeader(status)
}

// sidecarFailure is an HTTP status with one fixed S3 code. Zero means success.
// Messages come only from StatusText, never from requests or upstream errors.
type sidecarFailure int

var sidecarCodes = map[sidecarFailure]string{
	http.StatusBadRequest:                   "InvalidArgument",
	http.StatusForbidden:                    "AccessDenied",
	http.StatusNotFound:                     "NoSuchKey",
	http.StatusMethodNotAllowed:             "MethodNotAllowed",
	http.StatusPreconditionFailed:           "PreconditionFailed",
	http.StatusRequestedRangeNotSatisfiable: "InvalidRange",
	http.StatusRequestHeaderFieldsTooLarge:  "RequestHeaderSectionTooLarge",
	http.StatusInternalServerError:          "InternalError",
	http.StatusNotImplemented:               "NotImplemented",
	http.StatusBadGateway:                   "BadGateway",
	http.StatusServiceUnavailable:           "ServiceUnavailable",
}

func (f sidecarFailure) write(w http.ResponseWriter, r *http.Request) {
	// Both fields are fixed ASCII text without XML metacharacters.
	body := "<Error><Code>" + sidecarCodes[f] + "</Code><Message>" + http.StatusText(int(f)) + "</Message></Error>"

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(int(f))

	if r.Method != http.MethodHead {
		if _, err := w.Write([]byte(body)); err != nil {
			return
		}
	}
}

func sidecarSDKFailure(err error) sidecarFailure {
	switch {
	case errors.Is(err, racersdk.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, racersdk.ErrUnauthorized), errors.Is(err, racersdk.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, racersdk.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, racersdk.ErrVersionMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, racersdk.ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable
	case errors.Is(err, racersdk.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, net.ErrClosed):
		return http.StatusServiceUnavailable
	}

	return http.StatusBadGateway
}

func sidecarSupportedHeaders(h http.Header) bool {
	for name := range h {
		name = strings.ToLower(name)
		switch name {
		case "if-modified-since", "if-unmodified-since", "if-range":
			return false
		case "x-amz-date", "x-amz-content-sha256", "x-amz-security-token", "x-amz-region-set", "x-amz-s3session-token", "x-amz-user-agent":
			// These only carry ignored credentials, signing data, or SDK telemetry.
			continue
		}
		// Unknown S3 headers may change authorization or object-read semantics.
		// Reject even empty values rather than silently dropping those requirements.
		if strings.HasPrefix(name, "x-amz-") {
			return false
		}
	}

	return true
}

type sidecarEntityTag struct {
	value string
	weak  bool
}

type sidecarTags struct {
	present, wildcard bool
	tags              []sidecarEntityTag
}

func sidecarCondition(h http.Header, name string) (sidecarTags, bool) {
	values, present := h[http.CanonicalHeaderKey(name)]

	result := sidecarTags{present: present}
	if !present {
		return result, true
	}

	s := strings.Trim(strings.Join(values, ","), " \t")
	if s == "*" {
		result.wildcard = true
		return result, true
	}

	// Ignore empty list elements, but only recognize a wildcard on its own.
	for s = strings.TrimLeft(s, ", \t"); s != ""; s = strings.TrimLeft(s, ", \t") {
		var weak, quoted, closed bool

		s, weak = strings.CutPrefix(s, "W/")

		s, quoted = strings.CutPrefix(s, `"`)
		if !quoted {
			return result, false
		}

		// Commas inside a quoted tag are literal; backslashes are not escapes.
		var value string

		value, s, closed = strings.Cut(s, `"`)
		if !closed {
			return result, false
		}

		for _, c := range []byte(value) {
			if c < 0x21 || c == 0x7f {
				return result, false
			}
		}

		result.tags = append(result.tags, sidecarEntityTag{value: `"` + value + `"`, weak: weak})

		s = strings.TrimLeft(s, " \t")
		if s != "" && s[0] != ',' {
			return result, false
		}
	}

	return result, len(result.tags) != 0
}

func (c sidecarTags) matches(tag string, weak bool) bool {
	if c.wildcard {
		return true
	}

	for _, candidate := range c.tags {
		if candidate.value == tag && (weak || !candidate.weak) {
			return true
		}
	}

	return false
}

func sidecarRange(h http.Header, size uint64) (offset, length uint64, partial bool, status int) {
	values, present := h["Range"]
	if !present {
		return 0, size, false, 0
	}

	if len(values) != 1 {
		return 0, 0, false, http.StatusBadRequest
	}

	s, ok := strings.CutPrefix(strings.Trim(values[0], " \t"), "bytes=")
	if !ok {
		return 0, 0, false, http.StatusBadRequest
	}

	first, last, ok := strings.Cut(s, "-")
	if !ok || first == "" && last == "" {
		return 0, 0, false, http.StatusBadRequest
	}

	if first == "" {
		suffix, valid := sidecarRangeNumber(last)
		if !valid {
			return 0, 0, false, http.StatusBadRequest
		}

		if suffix == 0 || size == 0 {
			return 0, 0, false, http.StatusRequestedRangeNotSatisfiable
		}

		length = min(suffix, size)

		return size - length, length, true, 0
	}

	start, ok := sidecarRangeNumber(first)
	if !ok {
		return 0, 0, false, http.StatusBadRequest
	}

	end := uint64(0)
	if last != "" {
		end, ok = sidecarRangeNumber(last)
		if !ok || end < start {
			return 0, 0, false, http.StatusBadRequest
		}
	}

	if start >= size {
		return 0, 0, false, http.StatusRequestedRangeNotSatisfiable
	}

	if last == "" || end >= size {
		end = size - 1
	}

	return start, end - start + 1, true, 0
}

// Reject signs and whitespace rather than accepting anything but decimal digits.
func sidecarRangeNumber(s string) (uint64, bool) {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}

	n, err := strconv.ParseUint(s, 10, 64)

	return n, err == nil
}
