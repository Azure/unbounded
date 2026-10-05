// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const ObjectPrefix = "/v1/objects/"

const ClientObjectPrefix = "/v2/objects/"

const PageSize = 16 * 1024 * 1024

type Operation uint8

const (
	OperationHead Operation = iota + 1
	OperationBootstrap
	OperationPinned
)

// Request describes a validated origin operation without SDK types or credentials in diagnostics.
type Request struct {
	Key                            [32]byte
	Operation                      Operation
	Pin                            string
	Range                          Range
	AdapterMetadata, Authorization string
}

type Metadata struct {
	Size        uint64
	ETag        string
	ExpiresAt   time.Time
	ContentType string
}

func (m Metadata) Validate() error {
	if err := ValidateContentType(m.ContentType); err != nil {
		return err
	}

	if m.Size > math.MaxInt64 || ValidateETag(m.ETag) != nil {
		return failure(ErrorInvalidArgument, "metadata", nil)
	}

	sec := m.ExpiresAt.Unix()

	ms := int64(m.ExpiresAt.Nanosecond() / int(time.Millisecond))
	if sec < 0 || sec > math.MaxInt64/1000 || m.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 || sec == math.MaxInt64/1000 && ms > math.MaxInt64%1000 {
		return failure(ErrorInvalidArgument, "metadata", nil)
	}

	return nil
}

type Range struct {
	Present     bool
	First, Last uint64
}

func ClosedRange(first, last uint64) (Range, error) {
	if first > last || last > math.MaxInt64 {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return Range{Present: true, First: first, Last: last}, nil
}

// Resolve clips a closed range to an object; ResolvePage additionally enforces origin page shape.
func (r Range) Resolve(size uint64) (uint64, uint64, error) {
	if size > math.MaxInt64 || !r.Present || r.First > r.Last || r.Last > math.MaxInt64 {
		return 0, 0, failure(ErrorInvalidArgument, "range", nil)
	}

	if r.First >= size {
		return 0, 0, failure(ErrorUnsatisfiableRange, "range", nil)
	}

	return r.First, min(size-1, r.Last), nil
}

func ValidatePageShape(r Range) error {
	if !r.Present || r.First > r.Last || r.Last > math.MaxInt64 || r.First%PageSize != 0 || r.Last > NominalPageEnd(r.First) {
		return failure(ErrorInvalidArgument, "origin range", nil)
	}

	return nil
}

func NominalPageEnd(first uint64) uint64 {
	return first + min(uint64(PageSize)-1, uint64(math.MaxInt64)-first)
}

func (r Range) ResolvePage(size uint64) (uint64, uint64, error) {
	if err := ValidatePageShape(r); err != nil {
		return 0, 0, err
	}

	first, last, err := r.Resolve(size)
	if err != nil {
		return 0, 0, err
	}

	if r.Last != NominalPageEnd(r.First) && r.Last != size-1 {
		return 0, 0, failure(ErrorInvalidArgument, "origin range", nil)
	}

	return first, last, nil
}

func Decimal(s string) (uint64, error) {
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

func ParseRange(s string) (Range, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	first, last, ok := strings.Cut(s[len("bytes="):], "-")
	if !ok {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	a, err := Decimal(first)
	if err != nil {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	b, err := Decimal(last)
	if err != nil {
		return Range{}, failure(ErrorInvalidArgument, "range", nil)
	}

	return ClosedRange(a, b)
}

func RangeValue(r Range) string {
	if !r.Present {
		return ""
	}

	return "bytes=" + strconv.FormatUint(r.First, 10) + "-" + strconv.FormatUint(r.Last, 10)
}

type ContentRange struct {
	First, Last uint64
	Size        uint64
	Unsatisfied bool
}

func ParseContentRange(s string) (ContentRange, error) {
	bad := failure(ErrorProtocol, "content range", nil)
	if !strings.HasPrefix(s, "bytes ") {
		return ContentRange{}, bad
	}

	bounds, total, ok := strings.Cut(s[6:], "/")
	if !ok {
		return ContentRange{}, bad
	}

	n, err := Decimal(total)
	if err != nil {
		return ContentRange{}, bad
	}

	if bounds == "*" {
		return ContentRange{Size: n, Unsatisfied: true}, nil
	}

	a, b, ok := strings.Cut(bounds, "-")
	if !ok {
		return ContentRange{}, bad
	}

	first, err := Decimal(a)
	if err != nil {
		return ContentRange{}, bad
	}

	last, err := Decimal(b)
	if err != nil || first > last || last >= n {
		return ContentRange{}, bad
	}

	return ContentRange{First: first, Last: last, Size: n}, nil
}

func ForbiddenHeaders(h http.Header) bool {
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
func ParseRequestHead(head []byte, origin bool) (Request, error) {
	var result Request
	if err := ValidateRawHead(head, false); err != nil {
		return result, err
	}

	bad := failure(ErrorInvalidArgument, "request", nil)

	h := HeadHeaders(head)
	if ForbiddenHeaders(h) || len(h.Values("Host")) != 1 || h.Get("Host") != "racer" {
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
	if !ok || !valid || protocol != "HTTP/1.1" || len(target) != len(ObjectPrefix)+64 || !strings.HasPrefix(target, ObjectPrefix) {
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

	result.Key, err = ParseKey(target[len(ObjectPrefix):])
	if err != nil {
		return Request{}, bad
	}

	if values, ok := h["Racer-Metadata"]; ok {
		result.AdapterMetadata = values[0]

		err = ValidateOpaque(values[0])
		if err != nil {
			return Request{}, bad
		}
	}

	if values, ok := h["Authorization"]; ok {
		result.Authorization = values[0]

		err = ValidateOpaque(values[0])
		if err != nil {
			return Request{}, bad
		}
	}

	if values, ok := h["If-Match"]; ok {
		result.Pin = values[0]

		err = ValidateETag(values[0])
		if err != nil {
			return Request{}, bad
		}
	}

	if values, ok := h["Range"]; ok {
		result.Range, err = ParseRange(values[0])
		if err != nil {
			return Request{}, bad
		}
	}

	switch method {
	case "HEAD":
		if result.Range.Present {
			return Request{}, bad
		}

		result.Operation = OperationHead
	case "GET":
		if !result.Range.Present {
			return Request{}, bad
		}

		if result.Pin == "" {
			if result.Range != BootstrapRange() {
				return Request{}, bad
			}

			result.Operation = OperationBootstrap
		} else {
			result.Operation = OperationPinned
			if origin {
				if err := ValidatePageShape(result.Range); err != nil {
					return Request{}, err
				}
			}
		}
	default:
		return Request{}, &Error{Kind: ErrorInvalidArgument, Operation: "request", Status: 405}
	}

	return result, nil
}

func BootstrapRange() Range { return Range{Present: true, Last: PageSize - 1} }

func ValidateRequest(r Request) error {
	if r.Operation < OperationHead || r.Operation > OperationPinned {
		return failure(ErrorInvalidArgument, "request", nil)
	}
	// Validate private values directly instead of serializing and reparsing with
	// net/http. The latter allocates another buffered reader and header map on
	// every request, including already validated origin requests.
	for _, value := range []string{r.AdapterMetadata, r.Authorization} {
		if value != "" {
			if err := ValidateOpaque(value); err != nil {
				return err
			}
		}
	}

	if r.Pin != "" {
		if err := ValidateETag(r.Pin); err != nil {
			return err
		}
	}

	switch r.Operation {
	case OperationHead:
		if r.Range.Present {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	case OperationBootstrap:
		if r.Pin != "" || r.Range != BootstrapRange() {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	case OperationPinned:
		if r.Pin == "" || !r.Range.Present || r.Range.First > r.Range.Last || r.Range.Last > math.MaxInt64 {
			return failure(ErrorInvalidArgument, "request", nil)
		}
	}

	// Three variable fields, each bounded to 8 KiB, plus fixed framing and a
	// two-int64 range fit below the 32 KiB aggregate limit. No serialization is
	// needed to validate a request before capacity admission or network I/O.
	return nil
}

func RequestHeaders(r Request) http.Header {
	h := make(http.Header, 4)

	if r.Range.Present {
		h["Range"] = []string{RangeValue(r.Range)}
	}

	if r.Pin != "" {
		h["If-Match"] = []string{r.Pin}
	}

	if r.AdapterMetadata != "" {
		h["Racer-Metadata"] = []string{r.AdapterMetadata}
	}

	if r.Authorization != "" {
		h["Authorization"] = []string{r.Authorization}
	}

	return h
}

// requestHead serializes a validated operation directly to HTTP/1.1.
func RequestHead(r Request) ([]byte, error) {
	return requestHeadAt(r, ObjectPrefix)
}

// clientHead keeps Stat on the current client endpoint without changing origin callbacks.
func ClientHead(r Request) ([]byte, error) {
	if r.Operation != OperationHead {
		return nil, failure(ErrorInvalidArgument, "client HEAD", nil)
	}

	return requestHeadAt(r, ClientObjectPrefix)
}

// ParseClientHead translates the v2 HEAD endpoint without changing origin parsing.
func ParseClientHead(r *http.Request) (Request, error) {
	if r.Method != http.MethodHead || !strings.HasPrefix(r.RequestURI, ClientObjectPrefix) {
		return Request{}, failure(ErrorInvalidArgument, "request", nil)
	}

	var raw bytes.Buffer
	raw.WriteString(r.Method + " " + ObjectPrefix + strings.TrimPrefix(r.RequestURI, ClientObjectPrefix) + " HTTP/1.1\r\nHost: " + r.Host + "\r\n")

	if err := r.Header.Write(&raw); err != nil {
		return Request{}, err
	}

	raw.WriteString("\r\n")

	return ParseRequestHead(raw.Bytes(), false)
}

func requestHeadAt(r Request, prefix string) ([]byte, error) {
	if err := ValidateRequest(r); err != nil {
		return nil, err
	}

	method := "GET"
	if r.Operation == OperationHead {
		method = "HEAD"
	}

	var b strings.Builder
	b.WriteString(method + " " + prefix + hex.EncodeToString(r.Key[:]) + " HTTP/1.1\r\nHost: racer\r\n")

	if r.Range.Present {
		b.WriteString("Range: " + RangeValue(r.Range) + "\r\n")
	}

	if r.Pin != "" {
		b.WriteString("If-Match: " + r.Pin + "\r\n")
	}

	if r.AdapterMetadata != "" {
		b.WriteString("Racer-Metadata: " + r.AdapterMetadata + "\r\n")
	}

	if r.Authorization != "" {
		b.WriteString("Authorization: " + r.Authorization + "\r\n")
	}

	b.WriteString("\r\n")

	return []byte(b.String()), nil
}

type Response struct {
	Metadata    Metadata
	First, Last uint64
	Length      int64
	Close       bool
}

// parseResponseHead validates a success or empty protocol error against the exact
// request and optional initial metadata snapshot. A snapshot enforces immutable
// size/tag across continuation; refreshed expiry is permitted. For HEAD, length
// is zero although metadata.Size comes from Content-Length. No body is consumed.
func ParseResponseHead(head []byte, request Request, snapshot *Metadata) (Response, error) {
	var result Response
	if err := ValidateRawHead(head, true); err != nil {
		return result, err
	}

	bad := failure(ErrorProtocol, "response", nil)

	h := HeadHeaders(head)
	if ForbiddenHeaders(h) {
		return result, bad
	}

	length, err := Decimal(h.Get("Content-Length"))
	if err != nil {
		return result, bad
	}

	if request.Operation < OperationHead || request.Operation > OperationPinned {
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

	result.Close = ConnectionClose(h)

	if status != 200 && status != 206 {
		statusErr := StatusError(status)
		if statusErr.Kind == ErrorProtocol || length != 0 {
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
			cr, err := ParseContentRange(h.Get("Content-Range"))
			if err != nil || !cr.Unsatisfied || snapshot != nil && cr.Size != snapshot.Size {
				return result, bad
			}
		} else if _, ok := h["Content-Range"]; ok {
			return result, bad
		}

		if status == 405 && h.Get("Allow") != "HEAD, GET" {
			return result, bad
		}

		if request.Pin != "" && status == 404 {
			return result, bad
		}

		return result, statusErr
	}

	tag := h.Get("Etag")
	if err := ValidateETag(tag); err != nil {
		return result, bad
	}

	expiry, err := Decimal(h.Get("Racer-Expires-At"))
	if err != nil {
		return result, bad
	}

	contentType := h.Get("Racer-Content-Type")
	if values, present := h["Racer-Content-Type"]; present && (values[0] == "" || ValidateContentType(contentType) != nil) {
		return result, bad
	}

	result.Metadata = Metadata{ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC(), ContentType: contentType}
	if request.Pin != "" && tag != request.Pin {
		return Response{}, bad
	}

	if request.Operation == OperationHead {
		if status != 200 {
			return Response{}, bad
		}

		if _, ok := h["Content-Range"]; ok {
			return Response{}, bad
		}

		result.Metadata.Size = length
	} else {
		if h.Get("Content-Type") != "application/octet-stream" {
			return Response{}, bad
		}

		result.Length = int64(length)
		if status == 200 {
			if request.Operation != OperationBootstrap || length != 0 {
				return Response{}, bad
			}

			if _, ok := h["Content-Range"]; ok {
				return Response{}, bad
			}
		} else {
			cr, err := ParseContentRange(h.Get("Content-Range"))
			if err != nil || cr.Unsatisfied || cr.Last-cr.First+1 != length {
				return Response{}, bad
			}

			first, last, err := request.Range.Resolve(cr.Size)
			if err != nil || first != cr.First || last != cr.Last {
				return Response{}, bad
			}

			result.Metadata.Size, result.First, result.Last = cr.Size, first, last
		}
	}

	if snapshot != nil && (snapshot.Size != result.Metadata.Size || snapshot.ETag != result.Metadata.ETag || snapshot.ContentType != result.Metadata.ContentType) {
		return Response{}, bad
	}

	return result, nil
}

// metadataHeaders constructs the origin writer's metadata fields. Validate before
// UnixMilli (which otherwise silently overflows); the result contains no context.
func MetadataHeaders(m Metadata) (http.Header, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	h := http.Header{"Etag": {m.ETag}, "Racer-Expires-At": {strconv.FormatInt(m.ExpiresAt.UnixMilli(), 10)}}
	if m.ContentType != "" {
		h.Set("Racer-Content-Type", m.ContentType)
	}

	return h, nil
}

func ConnectionClose(h http.Header) bool {
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
func OriginResponse(request Request, m Metadata) (Response, error) {
	if err := m.Validate(); err != nil {
		return Response{}, failure(ErrorBadGateway, "origin metadata", err)
	}

	if request.Pin != "" && request.Pin != m.ETag {
		return Response{}, failure(ErrorBadGateway, "origin pin", nil)
	}

	result := Response{Metadata: m}

	switch request.Operation {
	case OperationHead:
		return result, nil
	case OperationBootstrap:
		if m.Size == 0 {
			return result, nil
		}
	case OperationPinned:
	default:
		return Response{}, failure(ErrorInvalidArgument, "origin operation", nil)
	}

	first, last, err := request.Range.ResolvePage(m.Size)
	if err != nil {
		return Response{}, err
	}

	result.First, result.Last, result.Length = first, last, int64(last-first)+1

	return result, nil
}

// contentRangeValue accepts already resolved bounds only.
func ContentRangeValue(first, last, size uint64) (string, error) {
	if size > math.MaxInt64 || first > last || last >= size {
		return "", failure(ErrorInvalidArgument, "content range", nil)
	}

	return "bytes " + strconv.FormatUint(uint64(first), 10) + "-" + strconv.FormatUint(uint64(last), 10) + "/" + strconv.FormatUint(uint64(size), 10), nil
}

// SubscriptionOptions carries the requested range, credits, and trusted snapshot.
type SubscriptionOptions struct {
	Offset, Length       uint64
	PageCredits          int
	ByteCredits          uint64
	Ordered, SmallObject bool
	Pin                  string
	Metadata             *Metadata
}

// SubscriptionHead builds the v2 POST envelope after SDK option validation.
func SubscriptionHead(r Request, o SubscriptionOptions) ([]byte, error) {
	var b strings.Builder

	ordered := 0
	if o.Ordered {
		ordered = 1
	}

	fmt.Fprintf(&b, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: %d\r\nRacer-Byte-Credits: %d\r\nRacer-Ordered: %d\r\n", hex.EncodeToString(r.Key[:]), o.PageCredits, o.ByteCredits, ordered)

	if o.Offset != 0 || o.Length != 0 {
		fmt.Fprintf(&b, "Range: bytes=%d-", o.Offset)

		if o.Length != 0 {
			fmt.Fprintf(&b, "%d", o.Offset+o.Length-1)
		}

		b.WriteString("\r\n")
	}

	for name, values := range RequestHeaders(r) {
		fmt.Fprintf(&b, "%s: %s\r\n", name, values[0])
	}

	b.WriteString("\r\n")
	head := []byte(b.String())

	return head, ValidateRawHead(head, false)
}

type SubscriptionResponse struct {
	Metadata          Metadata
	First, End, Pages uint64
}

func ParseSubscriptionResponse(head []byte, o SubscriptionOptions) (SubscriptionResponse, error) {
	var result SubscriptionResponse

	bad := failure(ErrorProtocol, "subscription response", nil)

	if err := ValidateRawHead(head, true); err != nil {
		return result, err
	}

	h := HeadHeaders(head)

	line := string(head[:bytes.Index(head, []byte("\r\n"))])
	if len(line) < 12 || !strings.HasPrefix(line, "HTTP/1.1 ") || len(line) > 12 && line[12] != ' ' || ForbiddenHeaders(h) {
		return result, bad
	}

	status, err := strconv.Atoi(line[9:12])
	if err != nil {
		return result, bad
	}

	length, err := Decimal(h.Get("Content-Length"))
	if err != nil {
		return result, bad
	}

	if status != 200 {
		if length != 0 {
			return result, bad
		}

		if status == 416 {
			value := h.Get("Content-Range")
			if !strings.HasPrefix(value, "bytes */") {
				return result, bad
			}

			if _, err := Decimal(strings.TrimPrefix(value, "bytes */")); err != nil {
				return result, bad
			}
		} else if h.Get("Content-Range") != "" {
			return result, bad
		}

		return result, StatusError(status)
	}

	if !ConnectionClose(h) || h.Get("Content-Range") != "" || h.Get("Content-Type") != "application/octet-stream" {
		return result, bad
	}

	size, err := Decimal(h.Get("Racer-Object-Length"))
	if err != nil {
		return result, bad
	}

	first, err := Decimal(h.Get("Racer-Range-Start"))
	if err != nil {
		return result, bad
	}

	end, err := Decimal(h.Get("Racer-Range-End"))
	if err != nil {
		return result, bad
	}

	expiry, err := Decimal(h.Get("Racer-Expires-At"))
	if err != nil {
		return result, bad
	}

	tag := h.Get("ETag")
	if ValidateETag(tag) != nil {
		return result, bad
	}

	m := Metadata{Size: size, ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC(), ContentType: h.Get("Racer-Content-Type")}
	if m.Validate() != nil || first > end || end > size || first != o.Offset || o.Length == 0 && end != size || o.Pin != "" && o.Pin != tag {
		return result, bad
	}

	if o.Length != 0 && end-first != o.Length {
		if end == size && o.Length > end-first {
			return result, failure(ErrorUnsatisfiableRange, "subscription range", nil)
		}

		return result, bad
	}

	if o.Metadata != nil && (o.Metadata.Size != m.Size || o.Metadata.ETag != m.ETag || o.Metadata.ContentType != m.ContentType) {
		return result, bad
	}

	if o.SmallObject && m.Size > PageSize {
		return result, failure(ErrorInvalidArgument, "small object size", nil)
	}

	pages := PageCount(first, end)
	if pages+1 > (math.MaxInt64-(end-first))/FrameSize || length != end-first+FrameSize*(pages+1) {
		return result, bad
	}

	if o.Metadata != nil {
		m = *o.Metadata
	}

	return SubscriptionResponse{Metadata: m, First: first, End: end, Pages: pages}, nil
}

type SubscriptionRequest struct {
	Request                  Request
	First, End               uint64
	Ranged, Ordered          bool
	PageCredits, ByteCredits uint64
}

// ParseSubscriptionRequest translates a normalized HTTP envelope for the fake peer.
// Raw socket servers must validate the head before net/http normalizes it.
func ParseSubscriptionRequest(r *http.Request) (SubscriptionRequest, error) {
	s := SubscriptionRequest{End: math.MaxInt64, PageCredits: 2, ByteCredits: 2 * PageSize}
	bad := failure(ErrorInvalidArgument, "fake subscription", nil)

	const prefix = ClientObjectPrefix
	if r.Method != http.MethodPost || r.Proto != "HTTP/1.1" || r.Host != "racer" || len(r.RequestURI) != len(prefix)+64 || !strings.HasPrefix(r.RequestURI, prefix) || r.Header.Get("Content-Length") != "0" || len(r.TransferEncoding) != 0 || ForbiddenHeaders(r.Header) {
		return s, bad
	}

	for _, name := range []string{"Content-Length", "If-Match", "Range", "Racer-Page-Credits", "Racer-Byte-Credits", "Racer-Ordered", "Racer-Metadata", "Authorization"} {
		if len(r.Header.Values(name)) > 1 {
			return s, bad
		}
	}

	for _, credit := range []struct {
		name     string
		dest     *uint64
		min, max uint64
	}{
		{"Racer-Page-Credits", &s.PageCredits, 1, 64},
		{"Racer-Byte-Credits", &s.ByteCredits, PageSize, 64 * PageSize},
	} {
		if values, ok := r.Header[credit.name]; ok {
			n, err := Decimal(values[0])
			if err != nil || n < credit.min || n > credit.max {
				return s, bad
			}

			*credit.dest = n
		}
	}

	if values, ok := r.Header["Racer-Ordered"]; ok && values[0] != "0" && values[0] != "1" {
		return s, bad
	}

	s.Ordered = r.Header.Get("Racer-Ordered") == "1"

	h := r.Header.Clone()
	if values, ok := h["Range"]; ok {
		s.Ranged = true

		value := values[0]
		if strings.HasPrefix(value, "bytes=") && strings.HasSuffix(value, "-") {
			first, err := Decimal(strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-"))
			if err != nil {
				return s, bad
			}

			s.First = first
		} else {
			bounds, err := ParseRange(value)
			if err != nil {
				return s, bad
			}

			s.First, s.End = bounds.First, bounds.Last+1
		}
	}

	h.Del("Range")

	var raw bytes.Buffer
	fmt.Fprintf(&raw, "HEAD %s%s HTTP/1.1\r\nHost: racer\r\n", ObjectPrefix, r.RequestURI[len(prefix):])

	if err := h.Write(&raw); err != nil {
		return s, err
	}

	raw.WriteString("\r\n")
	request, err := ParseRequestHead(raw.Bytes(), false)
	s.Request = request

	return s, err
}

// WriteSubscriptionHead writes success or error headers and forces connection closure.
func WriteSubscriptionHead(w io.Writer, status int, h http.Header) error {
	h.Set("Connection", "close")

	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}

	if err := h.Write(w); err != nil {
		return err
	}

	_, err := io.WriteString(w, "\r\n")

	return err
}

// SubscriptionHeaders builds a bounded success envelope; oversized framing is rejected.
func SubscriptionHeaders(m Metadata, first, end uint64) (http.Header, error) {
	length, pages := end-first, PageCount(first, end)
	if pages+1 > (math.MaxInt64-length)/FrameSize {
		return nil, failure(ErrorProtocol, "subscription response", nil)
	}

	h, err := MetadataHeaders(m)
	if err != nil {
		return nil, err
	}

	h.Set("Content-Type", "application/octet-stream")
	h.Set("Racer-Object-Length", strconv.FormatUint(m.Size, 10))
	h.Set("Racer-Range-Start", strconv.FormatUint(first, 10))
	h.Set("Racer-Range-End", strconv.FormatUint(end, 10))
	h.Set("Content-Length", strconv.FormatUint(length+FrameSize*(pages+1), 10))

	return h, nil
}
