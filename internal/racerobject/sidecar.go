// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"encoding/xml"
	"errors"
	"fmt"
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
	client    *racersdk.Client
	namespace string
	buckets   map[string]struct{}
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

	if _, err := NewRequest(config.Namespace, "validation", "validation", ""); err != nil {
		return nil, errors.New("racer-object: invalid sidecar namespace")
	}

	s := &sidecar{client: client, namespace: config.Namespace, buckets: make(map[string]struct{}, len(config.Buckets))}
	for _, bucket := range config.Buckets {
		if _, err := NewRequest(config.Namespace, bucket, "validation", ""); err != nil {
			return nil, errors.New("racer-object: invalid sidecar bucket")
		}

		s.buckets[bucket] = struct{}{}
	}

	return s, nil
}

func (s *sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		sidecarError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")

		return
	}

	request, version, code := s.request(r)
	if code != "" {
		status := http.StatusBadRequest
		if code == "NotImplemented" {
			status = http.StatusNotImplemented
		}

		if code == "AccessDenied" {
			status = http.StatusForbidden
		}

		sidecarError(w, r, status, code)

		return
	}

	match, ok := sidecarCondition(r.Header, "If-Match")
	if !ok {
		sidecarError(w, r, http.StatusBadRequest, "InvalidArgument")
		return
	}

	none, ok := sidecarCondition(r.Header, "If-None-Match")
	if !ok {
		sidecarError(w, r, http.StatusBadRequest, "InvalidArgument")
		return
	}

	m, err := s.client.Stat(r.Context(), request)
	if err != nil {
		sidecarSDKError(w, r, err)
		return
	}

	if match.present && !match.matches(m.ETag.String(), false) {
		sidecarError(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}

	if none.present && none.matches(m.ETag.String(), true) {
		w.Header().Set("ETag", m.ETag.String())
		w.WriteHeader(http.StatusNotModified)

		return
	}

	offset, length, partial, status := sidecarRange(r.Header, uint64(m.Size))
	if status != 0 {
		code := "InvalidArgument"

		if status == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", m.Size))

			code = "InvalidRange"
		}

		sidecarError(w, r, status, code)

		return
	}

	status = http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}

	if r.Method == http.MethodHead {
		sidecarObjectHeaders(w.Header(), m, version, offset, length, partial)
		w.WriteHeader(status)

		return
	}

	v, err := s.client.GetStreaming(r.Context(), request, racersdk.ReadOptions{
		Metadata: &m, Offset: racersdk.ByteOffset(offset), Length: racersdk.ByteLength(length),
	})
	if err != nil {
		sidecarSDKError(w, r, err)
		return
	}

	defer func() {
		if closeErr := v.Close(); closeErr != nil {
			return
		}
	}()
	// Empty success is not committed until the SDK has validated Complete.
	if length == 0 {
		if _, err := v.WriteToHTTP(w); err != nil {
			sidecarSDKError(w, r, err)
			return
		}

		sidecarObjectHeaders(w.Header(), m, version, offset, length, partial)
		w.WriteHeader(status)

		return
	}

	sidecarObjectHeaders(w.Header(), m, version, offset, length, partial)
	w.WriteHeader(status)

	if _, err := v.WriteToHTTP(w); err != nil {
		// Never append XML to a partial object or turn a failed stream into EOF.
		panic(http.ErrAbortHandler)
	}
}

func (s *sidecar) request(r *http.Request) (racersdk.Request, string, string) {
	if !sidecarSupportedHeaders(r.Header) {
		return racersdk.Request{}, "", "NotImplemented"
	}
	// Split before decoding so an escaped slash is key data, not a bucket boundary.
	// Decode each part exactly once; never clean dot segments or repeated slashes.
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), "/")

	bucket, key, found := strings.Cut(path, "/")
	if !ok || !found || bucket == "" || key == "" {
		return racersdk.Request{}, "", "InvalidArgument"
	}

	bucket, err := url.PathUnescape(bucket)
	if err != nil {
		return racersdk.Request{}, "", "InvalidArgument"
	}

	key, err = url.PathUnescape(key)
	if err != nil {
		return racersdk.Request{}, "", "InvalidArgument"
	}

	if len(s.buckets) != 0 {
		if _, ok := s.buckets[bucket]; !ok {
			return racersdk.Request{}, "", "AccessDenied"
		}
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return racersdk.Request{}, "", "InvalidArgument"
	}

	for name, values := range query {
		if len(values) != 1 || values[0] == "" {
			return racersdk.Request{}, "", "InvalidArgument"
		}

		switch name {
		case "versionId":
		case "x-id":
			want := "GetObject"
			if r.Method == http.MethodHead {
				want = "HeadObject"
			}

			if values[0] != want {
				return racersdk.Request{}, "", "NotImplemented"
			}
		default:
			return racersdk.Request{}, "", "NotImplemented"
		}
	}

	version := query.Get("versionId")
	for _, c := range []byte(version) {
		if c < 0x20 || c == 0x7f {
			return racersdk.Request{}, "", "InvalidArgument"
		}
	}

	request, err := NewRequest(s.namespace, bucket, key, version)
	if err != nil {
		return racersdk.Request{}, "", "InvalidArgument"
	}

	return request, version, ""
}

func sidecarObjectHeaders(h http.Header, m racersdk.Metadata, version string, offset, length uint64, partial bool) {
	h.Set("ETag", m.ETag.String())
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatUint(length, 10))

	contentType := m.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	h.Set("Content-Type", contentType)

	if version != "" {
		h.Set("x-amz-version-id", version)
	}

	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, m.Size))
	}
}

func sidecarError(w http.ResponseWriter, r *http.Request, status int, code string) {
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: http.StatusText(status)})
	if err != nil {
		panic(http.ErrAbortHandler)
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)

	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			return
		}
	}
}

func sidecarSDKError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusBadGateway, "BadGateway"

	var sdkErr *racersdk.Error
	if errors.As(err, &sdkErr) {
		switch sdkErr.Kind() {
		case racersdk.ErrorInvalidArgument:
			status, code = http.StatusBadRequest, "InvalidArgument"
		case racersdk.ErrorUnauthorized, racersdk.ErrorForbidden:
			status, code = http.StatusForbidden, "AccessDenied"
		case racersdk.ErrorNotFound:
			status, code = http.StatusNotFound, "NoSuchKey"
		case racersdk.ErrorVersionUnavailable:
			status, code = http.StatusPreconditionFailed, "PreconditionFailed"
		case racersdk.ErrorUnsatisfiableRange:
			status, code = http.StatusRequestedRangeNotSatisfiable, "InvalidRange"
		case racersdk.ErrorHeaderLimit:
			status, code = http.StatusRequestHeaderFieldsTooLarge, "RequestHeaderSectionTooLarge"
		case racersdk.ErrorInternal:
			status, code = http.StatusInternalServerError, "InternalError"
		case racersdk.ErrorClosed, racersdk.ErrorUnavailable, racersdk.ErrorCanceled, racersdk.ErrorDeadline:
			status, code = http.StatusServiceUnavailable, "ServiceUnavailable"
		}
	}

	sidecarError(w, r, status, code)
}
