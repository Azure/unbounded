// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const objectPrefix = "/v1/objects/"

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
	if !ok || first == "" && last == "" {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	var (
		a, b uint64
		err  error
	)

	if first != "" {
		a, err = decimal(first)
		if err != nil {
			return Range{}, failure(ErrorInvalidArgument, "range", nil)
		}
	}

	if last != "" {
		b, err = decimal(last)
		if err != nil {
			return Range{}, failure(ErrorInvalidArgument, "range", nil)
		}
	}

	if first == "" {
		return SuffixRange(ByteLength(b))
	}

	if last == "" {
		return FromRange(ByteOffset(a))
	}

	return ClosedRange(ByteOffset(a), ByteOffset(b))
}

func rangeValue(r Range) string {
	switch r.kind {
	case RangeClosed:
		return "bytes=" + strconv.FormatUint(r.first, 10) + "-" + strconv.FormatUint(r.last, 10)
	case RangeFrom:
		return "bytes=" + strconv.FormatUint(r.first, 10) + "-"
	case RangeSuffix:
		return "bytes=-" + strconv.FormatUint(r.last, 10)
	default:
		return ""
	}
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

	for _, name := range []string{"Content-Range", "Etag", "Racer-Expires-At"} {
		if _, ok := h[name]; ok {
			return result, bad
		}
	}

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		return result, bad
	}

	if req.Proto != "HTTP/1.1" || len(req.RequestURI) != len(objectPrefix)+64 || !strings.HasPrefix(req.RequestURI, objectPrefix) {
		return result, bad
	}

	result.key, err = ParseKey(req.RequestURI[len(objectPrefix):])
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

	switch req.Method {
	case "HEAD":
		if result.byteRange.kind != 0 {
			return OriginRequest{}, bad
		}

		result.operation = OperationHead
	case "GET":
		if result.byteRange.kind == 0 {
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

func bootstrapRange() Range { return Range{kind: RangeClosed, last: uint64(PageSize) - 1} }

// requestHead constructs canonical wire bytes and checks the aggregate limit.
// Callers build private operation descriptors; public Request has no pin/range.
func requestHead(r OriginRequest) ([]byte, error) {
	method := "GET"
	if r.operation == OperationHead {
		method = "HEAD"
	}

	if r.operation < OperationHead || r.operation > OperationPinned {
		return nil, failure(ErrorInvalidArgument, "request", nil)
	}

	var b strings.Builder
	b.WriteString(method + " " + objectPrefix + r.key.String() + " HTTP/1.1\r\nHost: racer\r\n")

	if r.byteRange.kind != 0 {
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
	head := []byte(b.String())

	parsed, err := parseRequestHead(head, false)
	if err != nil {
		return nil, err
	}

	if parsed.operation != r.operation {
		return nil, failure(ErrorInvalidArgument, "request", nil)
	}

	return head, nil
}

type wireResponse struct {
	metadata    Metadata
	first, last ByteOffset
	length      int64
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

	method := "GET"
	if request.operation == OperationHead {
		method = "HEAD"
	}

	if request.operation < OperationHead || request.operation > OperationPinned {
		return result, bad
	}

	res, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(head)), &http.Request{Method: method})
	if err != nil || res.Proto != "HTTP/1.1" {
		return result, bad
	}

	if res.StatusCode != 200 && res.StatusCode != 206 {
		statusErr := statusError(res.StatusCode)
		if statusErr.kind == ErrorProtocol || length != 0 {
			return result, bad
		}

		if _, ok := h["Etag"]; ok {
			return result, bad
		}

		if _, ok := h["Racer-Expires-At"]; ok {
			return result, bad
		}

		if res.StatusCode == 416 {
			cr, err := parseContentRange(h.Get("Content-Range"))
			if err != nil || !cr.unsatisfied || snapshot != nil && cr.size != snapshot.Size {
				return result, bad
			}
		} else if _, ok := h["Content-Range"]; ok {
			return result, bad
		}

		if res.StatusCode == 405 && h.Get("Allow") != "HEAD, GET" {
			return result, bad
		}

		if request.pin.value != "" && res.StatusCode == 404 {
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

	result.metadata = Metadata{ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC()}
	if request.pin.value != "" && tag != request.pin {
		return wireResponse{}, bad
	}

	if request.operation == OperationHead {
		if res.StatusCode != 200 {
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
		if res.StatusCode == 200 {
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

			first, last, err := request.byteRange.Resolve(cr.size)
			if err != nil || first != cr.first || last != cr.last {
				return wireResponse{}, bad
			}

			result.metadata.Size, result.first, result.last = cr.size, first, last
		}
	}

	if snapshot != nil && (snapshot.Size != result.metadata.Size || snapshot.ETag != result.metadata.ETag) {
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

	return http.Header{"Etag": {m.ETag.value}, "Racer-Expires-At": {strconv.FormatInt(m.ExpiresAt.UnixMilli(), 10)}}, nil
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

	first, last, err := resolveOriginRange(request.byteRange, m.Size)
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
