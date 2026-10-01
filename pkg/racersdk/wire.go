// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const objectPrefix = "/v1/objects/"

const clientObjectPrefix = "/v2/objects/"

func decimal(s string) (uint64, error) {
	if s == "" || len(s) > 19 || len(s) > 1 && s[0] == '0' {
		return 0, failure(ErrorProtocol, "decimal", nil)
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, failure(ErrorProtocol, "decimal", nil)
		}
	}

	n, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, failure(ErrorProtocol, "decimal", nil)
	}

	return n, nil
}

func parseRange(s string) (Range, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	first, last, ok := strings.Cut(s[len("bytes="):], "-")
	if !ok {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	a, err := decimal(first)
	if err != nil {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	b, err := decimal(last)
	if err != nil {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return ClosedRange(ByteOffset(a), ByteOffset(b))
}

func rangeValue(r Range) string {
	if !r.present {
		return ""
	}

	return "bytes=" + strconv.FormatUint(r.first, 10) + "-" + strconv.FormatUint(r.last, 10)
}

type contentRange struct {
	first, last ByteOffset
	size        ByteLength
	unsatisfied bool
}

func parseContentRange(s string) (contentRange, error) {
	bad := failure(ErrorProtocol, "content range", nil)
	if !strings.HasPrefix(s, "bytes ") {
		return contentRange{}, bad
	}

	bounds, total, ok := strings.Cut(s[6:], "/")
	if !ok {
		return contentRange{}, bad
	}

	n, err := decimal(total)
	if err != nil {
		return contentRange{}, bad
	}

	if bounds == "*" {
		return contentRange{size: ByteLength(n), unsatisfied: true}, nil
	}

	a, b, ok := strings.Cut(bounds, "-")
	if !ok {
		return contentRange{}, bad
	}

	first, err := decimal(a)
	if err != nil {
		return contentRange{}, bad
	}

	last, err := decimal(b)
	if err != nil || first > last || last >= n {
		return contentRange{}, bad
	}

	return contentRange{first: ByteOffset(first), last: ByteOffset(last), size: ByteLength(n)}, nil
}

func forbiddenHeaders(h http.Header) bool {
	for _, name := range []string{"Transfer-Encoding", "Content-Encoding", "Trailer", "Upgrade", "Expect", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if _, ok := h[name]; ok {
			return true
		}
	}

	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}

	return false
}

// parseRequestHead validates a complete raw head, without reading any body.
// origin=true adds whole-page shape rules; false permits dataplane ranges.
// The returned private OriginRequest also serves as the internal wire operation
// descriptor for bootstrap, HEAD, and pinned continuation response validation.
func parseRequestHead(head []byte, origin bool) (OriginRequest, error) {
	var result OriginRequest
	if err := validateRawHead(head, false); err != nil {
		return result, err
	}

	bad := failure(ErrorInvalidArgument, "request", nil)

	h := headHeaders(head)
	if forbiddenHeaders(h) || len(h.Values("Host")) != 1 || h.Get("Host") != "racer" {
		return result, bad
	}

	if values, ok := h["Content-Length"]; ok && (len(values) != 1 || values[0] != "0") {
		return result, bad
	}

	for _, name := range []string{"Content-Range", "Etag", "Racer-Expires-At", "Racer-Content-Type"} {
		if _, ok := h[name]; ok {
			return result, bad
		}
	}

	line := string(head[:bytes.Index(head, []byte("\r\n"))])
	method, rest, ok := strings.Cut(line, " ")

	target, protocol, valid := strings.Cut(rest, " ")
	if !ok || !valid || protocol != "HTTP/1.1" || len(target) != len(objectPrefix)+64 || !strings.HasPrefix(target, objectPrefix) {
		return result, bad
	}

	if method == "" {
		return result, bad
	}

	for i := range len(method) {
		if !headerToken(method[i]) {
			return result, bad
		}
	}

	var err error

	result.key, err = ParseKey(target[len(objectPrefix):])
	if err != nil {
		return OriginRequest{}, bad
	}

	if values, ok := h["Racer-Metadata"]; ok {
		result.context.metadata, err = ParseAdapterMetadata(values[0])
		if err != nil {
			return OriginRequest{}, bad
		}
	}

	if values, ok := h["Authorization"]; ok {
		result.context.authorization, err = ParseAuthorization(values[0])
		if err != nil {
			return OriginRequest{}, bad
		}
	}

	if values, ok := h["If-Match"]; ok {
		result.pin, err = ParseETag(values[0])
		if err != nil {
			return OriginRequest{}, bad
		}
	}

	if values, ok := h["Range"]; ok {
		result.byteRange, err = parseRange(values[0])
		if err != nil {
			return OriginRequest{}, bad
		}
	}

	switch method {
	case "HEAD":
		if result.byteRange.present {
			return OriginRequest{}, bad
		}

		result.operation = OperationHead
	case "GET":
		if !result.byteRange.present {
			return OriginRequest{}, bad
		}

		if result.pin.value == "" {
			if result.byteRange != bootstrapRange() {
				return OriginRequest{}, bad
			}

			result.operation = OperationBootstrap
		} else {
			result.operation = OperationPinned
			if origin {
				if err := validatePageShape(result.byteRange); err != nil {
					return OriginRequest{}, err
				}
			}
		}
	default:
		return OriginRequest{}, &Error{kind: ErrorInvalidArgument, operation: "request", status: 405}
	}

	return result, nil
}

func bootstrapRange() Range { return Range{present: true, last: uint64(PageSize) - 1} }

func validateRequest(r OriginRequest) error {
	if r.operation < OperationHead || r.operation > OperationPinned {
		return failure(ErrorInvalidArgument, "request", nil)
	}
	// Validate private values directly instead of serializing and reparsing with
	// net/http. The latter allocates another buffered reader and header map on
	// every request, including already validated origin requests.
	if _, err := NewFetchContext(r.context.metadata, r.context.authorization); err != nil {
		return err
	}

	if r.pin.value != "" {
		if _, err := ParseETag(r.pin.value); err != nil {
			return err
		}
	}

	switch r.operation {
	case OperationHead:
		if r.byteRange.present {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	case OperationBootstrap:
		if r.pin.value != "" || r.byteRange != bootstrapRange() {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	case OperationPinned:
		if r.pin.value == "" || !r.byteRange.present || r.byteRange.first > r.byteRange.last || r.byteRange.last > math.MaxInt64 {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	}

	// Three variable fields, each bounded to 8 KiB, plus fixed framing and a
	// two-int64 range fit below the 32 KiB aggregate limit. No serialization is
	// needed to validate a request before capacity admission or network I/O.
	return nil
}

func requestHeaders(r OriginRequest) http.Header {
	h := make(http.Header, 4)

	if r.byteRange.present {
		h["Range"] = []string{rangeValue(r.byteRange)}
	}

	if r.pin.value != "" {
		h["If-Match"] = []string{r.pin.value}
	}

	if r.context.metadata.value != "" {
		h["Racer-Metadata"] = []string{r.context.metadata.value}
	}

	if r.context.authorization.value != "" {
		h["Authorization"] = []string{r.context.authorization.value}
	}

	return h
}

// requestHead serializes a validated operation directly to HTTP/1.1.
func requestHead(r OriginRequest) ([]byte, error) {
	return requestHeadAt(r, objectPrefix)
}

// clientHead keeps Stat on the current client endpoint without changing origin callbacks.
func clientHead(r OriginRequest) ([]byte, error) {
	if r.operation != OperationHead {
		return nil, failure(ErrorInvalidArgument, "client HEAD", nil)
	}

	return requestHeadAt(r, clientObjectPrefix)
}

func requestHeadAt(r OriginRequest, prefix string) ([]byte, error) {
	if err := validateRequest(r); err != nil {
		return nil, err
	}

	method := "GET"
	if r.operation == OperationHead {
		method = "HEAD"
	}

	var b strings.Builder
	b.WriteString(method + " " + prefix + r.key.String() + " HTTP/1.1\r\nHost: racer\r\n")

	if r.byteRange.present {
		b.WriteString("Range: " + rangeValue(r.byteRange) + "\r\n")
	}

	if r.pin.value != "" {
		b.WriteString("If-Match: " + r.pin.value + "\r\n")
	}

	if r.context.metadata.value != "" {
		b.WriteString("Racer-Metadata: " + r.context.metadata.value + "\r\n")
	}

	if r.context.authorization.value != "" {
		b.WriteString("Authorization: " + r.context.authorization.value + "\r\n")
	}

	b.WriteString("\r\n")

	return []byte(b.String()), nil
}

type wireResponse struct {
	metadata    Metadata
	first, last ByteOffset
	length      int64
	close       bool
}

// parseResponseHead validates a success or empty protocol error against the exact
// request and optional initial metadata snapshot. A snapshot enforces immutable
// size/tag across continuation; refreshed expiry is permitted. For HEAD, length
// is zero although metadata.Size comes from Content-Length. No body is consumed.
func parseResponseHead(head []byte, request OriginRequest, snapshot *Metadata) (wireResponse, error) {
	var result wireResponse
	if err := validateRawHead(head, true); err != nil {
		return result, err
	}

	bad := failure(ErrorProtocol, "response", nil)

	h := headHeaders(head)
	if forbiddenHeaders(h) {
		return result, bad
	}

	length, err := decimal(h.Get("Content-Length"))
	if err != nil {
		return result, bad
	}

	if request.operation < OperationHead || request.operation > OperationPinned {
		return result, bad
	}

	line := head[:bytes.Index(head, []byte("\r\n"))]
	if len(line) < 12 || !bytes.HasPrefix(line, []byte("HTTP/1.1 ")) || len(line) > 12 && line[12] != ' ' {
		return result, bad
	}

	status := 0

	for _, b := range line[9:12] {
		if b < '0' || b > '9' {
			return result, bad
		}

		status = status*10 + int(b-'0')
	}

	result.close = connectionClose(h)

	if status != 200 && status != 206 {
		statusErr := statusError(status)
		if statusErr.kind == ErrorProtocol || length != 0 {
			return result, bad
		}

		if _, ok := h["Etag"]; ok {
			return result, bad
		}

		if _, ok := h["Racer-Expires-At"]; ok {
			return result, bad
		}

		if _, ok := h["Racer-Content-Type"]; ok {
			return result, bad
		}

		if status == 416 {
			cr, err := parseContentRange(h.Get("Content-Range"))
			if err != nil || !cr.unsatisfied || snapshot != nil && cr.size != snapshot.Size {
				return result, bad
			}
		} else if _, ok := h["Content-Range"]; ok {
			return result, bad
		}

		if status == 405 && h.Get("Allow") != "HEAD, GET" {
			return result, bad
		}

		if request.pin.value != "" && status == 404 {
			return result, bad
		}

		return result, statusErr
	}

	tag, err := ParseETag(h.Get("Etag"))
	if err != nil {
		return result, bad
	}

	expiry, err := decimal(h.Get("Racer-Expires-At"))
	if err != nil {
		return result, bad
	}

	contentType := h.Get("Racer-Content-Type")
	if values, present := h["Racer-Content-Type"]; present && (values[0] == "" || validateContentType(contentType) != nil) {
		return result, bad
	}

	result.metadata = Metadata{ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC(), ContentType: contentType}
	if request.pin.value != "" && tag != request.pin {
		return wireResponse{}, bad
	}

	if request.operation == OperationHead {
		if status != 200 {
			return wireResponse{}, bad
		}

		if _, ok := h["Content-Range"]; ok {
			return wireResponse{}, bad
		}

		result.metadata.Size = ByteLength(length)
	} else {
		if h.Get("Content-Type") != "application/octet-stream" {
			return wireResponse{}, bad
		}

		result.length = int64(length)
		if status == 200 {
			if request.operation != OperationBootstrap || length != 0 {
				return wireResponse{}, bad
			}

			if _, ok := h["Content-Range"]; ok {
				return wireResponse{}, bad
			}
		} else {
			cr, err := parseContentRange(h.Get("Content-Range"))
			if err != nil || cr.unsatisfied || uint64(cr.last-cr.first)+1 != length {
				return wireResponse{}, bad
			}

			first, last, err := request.byteRange.resolve(cr.size)
			if err != nil || first != cr.first || last != cr.last {
				return wireResponse{}, bad
			}

			result.metadata.Size, result.first, result.last = cr.size, first, last
		}
	}

	if snapshot != nil && (snapshot.Size != result.metadata.Size || snapshot.ETag != result.metadata.ETag || snapshot.ContentType != result.metadata.ContentType) {
		return wireResponse{}, bad
	}

	return result, nil
}

// metadataHeaders constructs the origin writer's metadata fields. Validate before
// UnixMilli (which otherwise silently overflows); the result contains no context.
func metadataHeaders(m Metadata) (http.Header, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	h := http.Header{"Etag": {m.ETag.value}, "Racer-Expires-At": {strconv.FormatInt(m.ExpiresAt.UnixMilli(), 10)}}
	if m.ContentType != "" {
		h.Set("Racer-Content-Type", m.ContentType)
	}

	return h, nil
}

func connectionClose(h http.Header) bool {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "close") {
				return true
			}
		}
	}

	return false
}

// originResponse validates successful callback metadata against the selected
// request before any response headers are written. Metadata/pin violations are
// 502 contracts; invalid partial pages are 400 and out-of-object pages are 416.
// The caller owns all body rules (including HEAD nil, empty bootstrap probe,
// nonempty body presence, final-byte holdback, and closing on every error).
func originResponse(request OriginRequest, m Metadata) (wireResponse, error) {
	if err := m.Validate(); err != nil {
		return wireResponse{}, failure(ErrorBadGateway, "origin metadata", err)
	}

	if request.pin.value != "" && request.pin != m.ETag {
		return wireResponse{}, failure(ErrorBadGateway, "origin pin", nil)
	}

	result := wireResponse{metadata: m}

	switch request.operation {
	case OperationHead:
		return result, nil
	case OperationBootstrap:
		if m.Size == 0 {
			return result, nil
		}
	case OperationPinned:
	default:
		return wireResponse{}, failure(ErrorInvalidArgument, "origin operation", nil)
	}

	first, last, err := request.byteRange.Resolve(m.Size)
	if err != nil {
		return wireResponse{}, err
	}

	result.first, result.last, result.length = first, last, int64(last-first)+1

	return result, nil
}

// contentRangeValue accepts already resolved bounds only.
func contentRangeValue(first, last ByteOffset, size ByteLength) (string, error) {
	if size > math.MaxInt64 || first > last || last >= ByteOffset(size) {
		return "", failure(ErrorInvalidArgument, "content range", nil)
	}

	return "bytes " + strconv.FormatUint(uint64(first), 10) + "-" + strconv.FormatUint(uint64(last), 10) + "/" + strconv.FormatUint(uint64(size), 10), nil
}
