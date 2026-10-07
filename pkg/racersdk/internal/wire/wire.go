// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire implements Racer HTTP heads, metadata grammar, and subscription framing.
// It owns no connections, deadlines, admission, or payload storage.
package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	MaxHeadBytes            = 32 * 1024
	MaxFieldBytes           = 8192
	ObjectPrefix            = "/v1/objects/"
	ClientObjectPrefix      = "/v2/objects/"
	PageSize                = 16 * 1024 * 1024
	FrameSize               = 21
	CreditSize              = 12
	PageFrame          byte = 1
	CompleteFrame      byte = 2
)

// ErrorKind classifies wire failures. The SDK maps kinds to its public
// sentinel errors.
type ErrorKind uint8

const (
	ErrorInvalidArgument ErrorKind = iota + 1
	// ErrorClosed is reserved to match the public SDK numbering; wire never produces it.
	ErrorClosed
	ErrorProtocol
	ErrorUnauthorized
	ErrorForbidden
	ErrorNotFound
	ErrorVersionUnavailable
	ErrorUnsatisfiableRange
	ErrorHeaderLimit
	ErrorInternal
	ErrorBadGateway
	ErrorUnavailable
	ErrorCanceled
	ErrorDeadline
	ErrorIO
)

// Error contains only a safe operation, classification, status, and inspectable cause.
type Error struct {
	Kind      ErrorKind
	Operation string
	Status    int
	Err       error
}

func (e *Error) Error() string { return "wire: " + e.Operation }
func (e *Error) Unwrap() error { return e.Err }

func failure(kind ErrorKind, op string, err error) *Error {
	return &Error{Kind: kind, Operation: op, Err: err}
}

func ioFailure(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}

	kind := ErrorIO
	if errors.Is(err, context.Canceled) {
		kind = ErrorCanceled
	}

	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorDeadline
	}

	return failure(kind, op, err)
}

func StatusError(status int) *Error {
	kind := ErrorProtocol

	switch status {
	case 400, 405:
		kind = ErrorInvalidArgument
	case 401:
		kind = ErrorUnauthorized
	case 403:
		kind = ErrorForbidden
	case 404:
		kind = ErrorNotFound
	case 412:
		kind = ErrorVersionUnavailable
	case 416:
		kind = ErrorUnsatisfiableRange
	case 431:
		kind = ErrorHeaderLimit
	case 500:
		kind = ErrorInternal
	case 502:
		kind = ErrorBadGateway
	case 503:
		kind = ErrorUnavailable
	}

	return &Error{Kind: kind, Operation: "response", Status: status}
}

func ParseKey(s string) ([32]byte, error) {
	var key [32]byte
	if len(s) != 64 {
		return key, failure(ErrorInvalidArgument, "key", nil)
	}

	for i := range len(s) {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return key, failure(ErrorInvalidArgument, "key", nil)
		}
	}

	if _, err := hex.Decode(key[:], []byte(s)); err != nil {
		return [32]byte{}, failure(ErrorInvalidArgument, "key", nil)
	}

	return key, nil
}

func ValidateETag(s string) error {
	if len(s) < 2 || len(s) > MaxFieldBytes || s[0] != '"' || s[len(s)-1] != '"' {
		return failure(ErrorInvalidArgument, "etag", nil)
	}

	for i := 1; i < len(s)-1; i++ {
		if s[i] != 0x21 && (s[i] < 0x23 || s[i] > 0x7e) {
			return failure(ErrorInvalidArgument, "etag", nil)
		}
	}

	return nil
}

func ValidateOpaque(s string) error {
	if len(s) > MaxFieldBytes {
		return failure(ErrorHeaderLimit, "context", nil)
	}

	if len(s) == 0 || s[0] == ' ' || s[len(s)-1] == ' ' {
		return failure(ErrorInvalidArgument, "context", nil)
	}

	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return failure(ErrorInvalidArgument, "context", nil)
		}
	}

	return nil
}

func ValidateContentType(s string) error {
	if s == "" {
		return nil
	}

	if len(s) > 256 || strings.TrimSpace(s) != s {
		return failure(ErrorInvalidArgument, "content type", nil)
	}

	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return failure(ErrorInvalidArgument, "content type", nil)
		}
	}

	rest := s
	token := func() string {
		i := 0
		for i < len(rest) && headerToken(rest[i]) {
			i++
		}

		value := rest[:i]
		rest = rest[i:]

		return value
	}
	consume := func(b byte) bool {
		if len(rest) == 0 || rest[0] != b {
			return false
		}

		rest = rest[1:]

		return true
	}

	bad := failure(ErrorInvalidArgument, "content type", nil)
	if token() == "" || !consume('/') || token() == "" {
		return bad
	}

	var parameters []string

	for rest != "" {
		rest = strings.TrimLeft(rest, " ")

		if !consume(';') {
			return bad
		}

		rest = strings.TrimLeft(rest, " ")

		name := token()
		if name == "" {
			return bad
		}

		for _, old := range parameters {
			if strings.EqualFold(old, name) {
				return bad
			}
		}

		parameters = append(parameters, name)
		rest = strings.TrimLeft(rest, " ")

		if !consume('=') {
			return bad
		}

		rest = strings.TrimLeft(rest, " ")

		if consume('"') {
			for {
				if rest == "" {
					return bad
				}

				b := rest[0]
				rest = rest[1:]

				if b == '"' {
					break
				}

				if b == '\\' {
					if rest == "" {
						return bad
					}

					rest = rest[1:]
				}
			}
		} else if token() == "" {
			return bad
		}
	}

	return nil
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

// ContentRangeValue accepts already resolved bounds only.
func ContentRangeValue(first, last, size uint64) (string, error) {
	if size > math.MaxInt64 || first > last || last >= size {
		return "", failure(ErrorInvalidArgument, "content range", nil)
	}

	return "bytes " + strconv.FormatUint(uint64(first), 10) + "-" + strconv.FormatUint(uint64(last), 10) + "/" + strconv.FormatUint(uint64(size), 10), nil
}

func BootstrapRange() Range { return Range{Present: true, Last: PageSize - 1} }

// ReadRawHead consumes one validated head from a connection-owned buffered reader.
// Keep using that reader for the body and next head: it may have read ahead.
// Memory is capped independently of line lengths. The caller owns deadlines,
// cancellation, sequential framing, and connection closure on any head error.
// Do not pool the returned bytes; they can contain upstream credentials.
func ReadRawHead(r *bufio.Reader, response bool) ([]byte, error) {
	head, err := ReadHeadBytes(r, response)
	if err != nil {
		return nil, err
	}

	if err := ValidateRawHead(head, response); err != nil {
		return nil, err
	}

	return head, nil
}

// ReadHeadBytes finds the bounded head; semantic parsers validate it separately.
func ReadHeadBytes(r *bufio.Reader, response bool) ([]byte, error) {
	head := make([]byte, 0, 1024)
	for len(head) < MaxHeadBytes {
		line, err := r.ReadSlice('\n')
		if len(line) > MaxHeadBytes-len(head) {
			return head, headFailure(response, true)
		}

		head = append(head, line...)

		if err == bufio.ErrBufferFull {
			continue
		}

		if err != nil {
			if err == io.EOF && len(head) != 0 {
				err = io.ErrUnexpectedEOF
			}

			return head, ioFailure("head read", err)
		}

		if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
			return head, nil
		}
	}

	return head, headFailure(response, true)
}

func headFailure(response, limit bool) error {
	kind := ErrorInvalidArgument
	if limit {
		kind = ErrorHeaderLimit
	}

	if response {
		kind = ErrorProtocol
	}

	return failure(kind, "wire head", nil)
}

// ValidateRawHead must run before net/http parsing. Parsed headers cannot recover
// trimmed context whitespace or duplicate Content-Length fields coalesced by net/http.
// This checks raw grammar and limits, not operation semantics; use ParseRequestHead
// or ParseResponseHead as well. Unknown fields still count toward the bound.
func ValidateRawHead(head []byte, response bool) error {
	if len(head) > MaxHeadBytes {
		return headFailure(response, true)
	}

	if !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		return headFailure(response, false)
	}

	lines := bytes.Split(head[:len(head)-4], []byte("\r\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		return headFailure(response, false)
	}

	for _, b := range lines[0] {
		if b < 0x20 || b == 0x7f {
			return headFailure(response, false)
		}
	}

	seen := make(map[string]bool)

	for _, line := range lines[1:] {
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return headFailure(response, false)
		}

		for _, b := range line[:colon] {
			if !headerToken(b) {
				return headFailure(response, false)
			}
		}

		name := strings.ToLower(string(line[:colon]))

		value := line[colon+1:]
		for _, b := range value {
			if b < 0x20 && b != '\t' || b == 0x7f {
				return headFailure(response, false)
			}
		}

		if singletonHeader(name) && seen[name] {
			return headFailure(response, false)
		}

		seen[name] = true
		if name == "racer-content-type" {
			if len(value) < 2 || value[0] != ' ' || ValidateContentType(string(value[1:])) != nil {
				return headFailure(response, false)
			}
		}

		if name == "racer-metadata" || name == "authorization" {
			if len(value) == 0 || value[0] != ' ' {
				return headFailure(response, false)
			}

			if err := ValidateOpaque(string(value[1:])); err != nil {
				return headFailure(response, len(value)-1 > MaxFieldBytes)
			}
		}
	}

	return nil
}

func headerToken(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(b))
}

func singletonHeader(name string) bool {
	switch name {
	case "host", "content-length", "content-type", "content-range", "etag", "if-match", "range",
		"racer-expires-at", "racer-content-type", "racer-metadata", "authorization", "racer-object-length",
		"racer-range-start", "racer-range-end", "racer-page-credits", "racer-byte-credits", "racer-ordered":
		return true
	default:
		return false
	}
}

// HeadHeaders applies standard HTTP whitespace normalization to a validated head.
// Call ValidateRawHead first so opaque context has already been checked byte for byte.
func HeadHeaders(head []byte) http.Header {
	h := make(http.Header)

	lines := bytes.Split(head[:len(head)-4], []byte("\r\n"))
	for _, line := range lines[1:] {
		i := bytes.IndexByte(line, ':')
		h.Add(string(line[:i]), strings.Trim(string(line[i+1:]), " \t"))
	}

	return h
}

func ForbiddenHeaders(h http.Header) bool {
	for _, name := range []string{
		"Transfer-Encoding", "Content-Encoding", "Trailer", "Upgrade", "Expect",
		"If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
	} {
		if _, ok := h[name]; ok {
			return true
		}
	}

	return connectionHasToken(h, "upgrade")
}

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
	AdapterMetadata, Authorization string `json:"-"`
}

// Format redacts request data using the SDK's diagnostic convention.
func (r Request) Format(s fmt.State, _ rune) {
	// fmt.State cannot usefully report a writer error back through Format.
	if _, err := io.WriteString(s, "Request([redacted])"); err != nil {
		return
	}
}

// ParseRequestHead validates a complete raw head without reading the body.
// When origin is true, pinned requests must follow whole-page shape rules.
// The returned descriptor also supplies the request for response validation.
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

	if hasAnyHeader(h, "Content-Range", "Etag", "Racer-Expires-At", "Racer-Content-Type") {
		return result, bad
	}

	method, key, ok := requestLine(head)
	if !ok {
		return result, bad
	}

	result, err := requestFields(h)
	if err != nil {
		return Request{}, bad
	}

	result.Key = key
	if err := result.selectOperation(method, origin); err != nil {
		return Request{}, err
	}

	return result, nil
}

func hasAnyHeader(h http.Header, names ...string) bool {
	for _, name := range names {
		if _, ok := h[name]; ok {
			return true
		}
	}

	return false
}

func requestLine(head []byte) (string, [32]byte, bool) {
	line := string(head[:bytes.Index(head, []byte("\r\n"))])
	method, rest, ok := strings.Cut(line, " ")

	target, protocol, valid := strings.Cut(rest, " ")
	if !ok || !valid || protocol != "HTTP/1.1" || len(target) != len(ObjectPrefix)+64 || !strings.HasPrefix(target, ObjectPrefix) {
		return "", [32]byte{}, false
	}

	if method == "" {
		return "", [32]byte{}, false
	}

	for i := range len(method) {
		if !headerToken(method[i]) {
			return "", [32]byte{}, false
		}
	}

	key, err := ParseKey(target[len(ObjectPrefix):])

	return method, key, err == nil
}

func requestFields(h http.Header) (Request, error) {
	var result Request

	for _, field := range []struct {
		name     string
		dest     *string
		validate func(string) error
	}{
		{"Racer-Metadata", &result.AdapterMetadata, ValidateOpaque},
		{"Authorization", &result.Authorization, ValidateOpaque},
		{"If-Match", &result.Pin, ValidateETag},
	} {
		if values, ok := h[field.name]; ok {
			*field.dest = values[0]
			if err := field.validate(values[0]); err != nil {
				return Request{}, err
			}
		}
	}

	if values, ok := h["Range"]; ok {
		var err error

		result.Range, err = ParseRange(values[0])
		if err != nil {
			return Request{}, err
		}
	}

	return result, nil
}

func (r *Request) selectOperation(method string, origin bool) error {
	bad := failure(ErrorInvalidArgument, "request", nil)

	switch method {
	case "HEAD":
		if r.Range.Present {
			return bad
		}

		r.Operation = OperationHead
	case "GET":
		if !r.Range.Present {
			return bad
		}

		if r.Pin == "" {
			if r.Range != BootstrapRange() {
				return bad
			}

			r.Operation = OperationBootstrap
		} else {
			r.Operation = OperationPinned
			if origin {
				return ValidatePageShape(r.Range)
			}
		}
	default:
		return &Error{Kind: ErrorInvalidArgument, Operation: "request", Status: 405}
	}

	return nil
}

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

// RequestHead validates and serializes an origin operation as HTTP/1.1.
func RequestHead(r Request) ([]byte, error) {
	return requestHeadAt(r, ObjectPrefix)
}

// ClientHead uses the v2 HEAD endpoint without changing origin callbacks.
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

// ParseResponseHead validates a success or empty protocol error against the exact
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
		return result, responseStatusError(h, status, length, request, snapshot)
	}

	m, ok := responseMetadata(h)
	if !ok {
		return result, bad
	}

	result.Metadata = m
	if request.Pin != "" && m.ETag != request.Pin {
		return Response{}, bad
	}

	if !result.resolveBody(h, status, length, request) || !sameSnapshot(snapshot, result.Metadata) {
		return Response{}, bad
	}

	return result, nil
}

func responseStatusError(h http.Header, status int, length uint64, request Request, snapshot *Metadata) error {
	bad := failure(ErrorProtocol, "response", nil)

	statusErr := StatusError(status)
	if statusErr.Kind == ErrorProtocol || length != 0 || hasAnyHeader(h, "Etag", "Racer-Expires-At", "Racer-Content-Type") {
		return bad
	}

	if status == 416 {
		cr, err := ParseContentRange(h.Get("Content-Range"))
		if err != nil || !cr.Unsatisfied || snapshot != nil && cr.Size != snapshot.Size {
			return bad
		}
	} else if hasAnyHeader(h, "Content-Range") {
		return bad
	}

	if status == 405 && h.Get("Allow") != "HEAD, GET" || request.Pin != "" && status == 404 {
		return bad
	}

	return statusErr
}

// responseMetadata reads the fields shared by HTTP and subscription responses.
// The caller supplies object size and maps invalid fields to its own operation.
func responseMetadata(h http.Header) (Metadata, bool) {
	tag := h.Get("Etag")
	if ValidateETag(tag) != nil {
		return Metadata{}, false
	}

	expiry, err := Decimal(h.Get("Racer-Expires-At"))
	if err != nil {
		return Metadata{}, false
	}

	contentType := h.Get("Racer-Content-Type")
	if values, present := h["Racer-Content-Type"]; present && (values[0] == "" || ValidateContentType(contentType) != nil) {
		return Metadata{}, false
	}

	return Metadata{ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC(), ContentType: contentType}, true
}

// sameSnapshot permits expiry refreshes but not changes to object identity or type.
func sameSnapshot(snapshot *Metadata, m Metadata) bool {
	return snapshot == nil || snapshot.Size == m.Size && snapshot.ETag == m.ETag && snapshot.ContentType == m.ContentType
}

func (r *Response) resolveBody(h http.Header, status int, length uint64, request Request) bool {
	if request.Operation == OperationHead {
		if status != 200 || hasAnyHeader(h, "Content-Range") {
			return false
		}

		r.Metadata.Size = length

		return true
	}

	if h.Get("Content-Type") != "application/octet-stream" {
		return false
	}

	r.Length = int64(length)
	if status == 200 {
		return request.Operation == OperationBootstrap && length == 0 && !hasAnyHeader(h, "Content-Range")
	}

	cr, err := ParseContentRange(h.Get("Content-Range"))
	if err != nil || cr.Unsatisfied || cr.Last-cr.First+1 != length {
		return false
	}

	first, last, err := request.Range.Resolve(cr.Size)
	if err != nil || first != cr.First || last != cr.Last {
		return false
	}

	r.Metadata.Size, r.First, r.Last = cr.Size, first, last

	return true
}

// MetadataHeaders constructs the origin writer's metadata fields. Validate before
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
	return connectionHasToken(h, "close")
}

func connectionHasToken(h http.Header, want string) bool {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}

	return false
}

// OriginResponse validates successful callback metadata against the selected
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

	fmt.Fprintf(&b, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"+
		"Racer-Page-Credits: %d\r\nRacer-Byte-Credits: %d\r\nRacer-Ordered: %d\r\n",
		hex.EncodeToString(r.Key[:]), o.PageCredits, o.ByteCredits, ordered)

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
		return result, subscriptionStatusError(h, status, length)
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

	m, ok := responseMetadata(h)
	if !ok {
		return result, bad
	}

	m.Size = size
	if err := validateSubscriptionSelection(m, first, end, o); err != nil {
		return result, err
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

// Subscription error envelopes intentionally have fewer restrictions than origin
// errors: unknown statuses are classified by StatusError, and empty range fields
// are allowed except for 416. Keep this separate from responseStatusError.
func subscriptionStatusError(h http.Header, status int, length uint64) error {
	bad := failure(ErrorProtocol, "subscription response", nil)
	if length != 0 {
		return bad
	}

	if status == 416 {
		value := h.Get("Content-Range")
		if !strings.HasPrefix(value, "bytes */") {
			return bad
		}

		if _, err := Decimal(strings.TrimPrefix(value, "bytes */")); err != nil {
			return bad
		}
	} else if h.Get("Content-Range") != "" {
		return bad
	}

	return StatusError(status)
}

func validateSubscriptionSelection(m Metadata, first, end uint64, o SubscriptionOptions) error {
	bad := failure(ErrorProtocol, "subscription response", nil)
	if m.Validate() != nil || first > end || end > m.Size || first != o.Offset || o.Length == 0 && end != m.Size || o.Pin != "" && o.Pin != m.ETag {
		return bad
	}

	if o.Length != 0 && end-first != o.Length {
		if end == m.Size && o.Length > end-first {
			return failure(ErrorUnsatisfiableRange, "subscription range", nil)
		}

		return bad
	}

	if !sameSnapshot(o.Metadata, m) {
		return bad
	}

	if o.SmallObject && m.Size > PageSize {
		return failure(ErrorInvalidArgument, "small object size", nil)
	}

	return nil
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
	if r.Method != http.MethodPost || r.Proto != "HTTP/1.1" || r.Host != "racer" ||
		len(r.RequestURI) != len(prefix)+64 || !strings.HasPrefix(r.RequestURI, prefix) ||
		r.Header.Get("Content-Length") != "0" || len(r.TransferEncoding) != 0 || ForbiddenHeaders(r.Header) {
		return s, bad
	}

	for _, name := range []string{
		"Content-Length", "If-Match", "Range", "Racer-Page-Credits", "Racer-Byte-Credits",
		"Racer-Ordered", "Racer-Metadata", "Authorization",
	} {
		if len(r.Header.Values(name)) > 1 {
			return s, bad
		}
	}

	if err := s.parseCredits(r.Header); err != nil {
		return s, err
	}

	h := r.Header.Clone()
	if values, ok := h["Range"]; ok {
		s.Ranged = true

		first, end, err := subscriptionRange(values[0])
		if err != nil {
			return s, bad
		}

		s.First, s.End = first, end
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

func (s *SubscriptionRequest) parseCredits(h http.Header) error {
	bad := failure(ErrorInvalidArgument, "fake subscription", nil)

	for _, credit := range []struct {
		name     string
		dest     *uint64
		min, max uint64
	}{
		{"Racer-Page-Credits", &s.PageCredits, 1, 64},
		{"Racer-Byte-Credits", &s.ByteCredits, PageSize, 64 * PageSize},
	} {
		if values, ok := h[credit.name]; ok {
			n, err := Decimal(values[0])
			if err != nil || n < credit.min || n > credit.max {
				return bad
			}

			*credit.dest = n
		}
	}

	if values, ok := h["Racer-Ordered"]; ok && values[0] != "0" && values[0] != "1" {
		return bad
	}

	s.Ordered = h.Get("Racer-Ordered") == "1"

	return nil
}

// subscriptionRange converts an inclusive or open HTTP range to exclusive bounds.
func subscriptionRange(value string) (uint64, uint64, error) {
	if strings.HasPrefix(value, "bytes=") && strings.HasSuffix(value, "-") {
		first, err := Decimal(strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-"))
		return first, math.MaxInt64, err
	}

	bounds, err := ParseRange(value)

	return bounds.First, bounds.Last + 1, err
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

// FrameReader streams a fixed-length body from the reader used by ReadRawHead.
// It never reads past the frame or closes its input. Zero length is immediately EOF.
// Truncation wraps io.ErrUnexpectedEOF; other errors with final bytes survive.
// Validate length first and do not reuse a failed or aborted frame. This reader
// cannot detect extra body bytes; callback bodies need the server's EOF probe.
type FrameReader struct {
	Source    io.Reader
	Remaining int64
}

func (r *FrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if r.Remaining == 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > r.Remaining {
		p = p[:r.Remaining]
	}

	n, err := r.Source.Read(p)

	r.Remaining -= int64(n)
	if err == io.EOF {
		if r.Remaining != 0 {
			err = io.ErrUnexpectedEOF
		} else {
			err = nil
		}
	}

	return n, ioFailure("body read", err)
}

// Frame describes a page payload or the terminal page/byte totals. Payload is external.
type Frame struct {
	Kind           byte
	Number, Offset uint64
	Length         uint32
}

func DecodeFrame(b [FrameSize]byte) Frame {
	return Frame{Kind: b[0], Number: binary.BigEndian.Uint64(b[1:9]), Offset: binary.BigEndian.Uint64(b[9:17]), Length: binary.BigEndian.Uint32(b[17:])}
}

func (f Frame) Encode() [FrameSize]byte {
	var b [FrameSize]byte

	b[0] = f.Kind
	binary.BigEndian.PutUint64(b[1:9], f.Number)
	binary.BigEndian.PutUint64(b[9:17], f.Offset)
	binary.BigEndian.PutUint32(b[17:], f.Length)

	return b
}

// WriteFrame preserves the encoder's single-write contract; the caller owns flushing.
func WriteFrame(w io.Writer, f Frame) error {
	b := f.Encode()
	_, err := w.Write(b[:])

	return err
}

type Credit struct {
	Number uint64
	Length uint32
}

func DecodeCredit(b [CreditSize]byte) Credit {
	return Credit{Number: binary.BigEndian.Uint64(b[:8]), Length: binary.BigEndian.Uint32(b[8:])}
}

func (c Credit) Encode() [CreditSize]byte {
	var b [CreditSize]byte
	binary.BigEndian.PutUint64(b[:8], c.Number)
	binary.BigEndian.PutUint32(b[8:], c.Length)

	return b
}

type pageInterval struct{ first, end uint64 }

// Sequence validates frame headers without reading payload, so callers can splice it.
// Call Accept only in the single receive goroutine, and do not continue after an error.
// Counts advance on accepted headers; callers must consume each payload before the next.
type Sequence struct {
	first, end, pages, delivered, bytes uint64
	ordered, complete                   bool
	intervals                           []pageInterval
}

func NewSequence(first, end uint64, ordered bool) *Sequence {
	return &Sequence{first: first, end: end, pages: PageCount(first, end), ordered: ordered}
}

// Intervals reports bounded tracking storage for diagnostics after a receive operation.
func (s *Sequence) Intervals() int {
	if s == nil {
		return 0
	}

	return len(s.intervals)
}

func PageCount(first, end uint64) uint64 {
	if end <= first {
		return 0
	}

	return (end-1)/PageSize - first/PageSize + 1
}

// Accept checks page identity, shape, uniqueness, ordering, and terminal totals.
// The operation is supplied to preserve the two SDK consumers' historical diagnostics.
func (s *Sequence) Accept(f Frame, operation string) error {
	bad := failure(ErrorProtocol, operation, nil)
	if s.complete {
		return bad
	}

	if f.Kind == CompleteFrame {
		if f.Number != s.pages || s.delivered != s.pages || f.Offset != s.end-s.first || s.bytes != f.Offset || f.Length != 0 {
			return bad
		}

		s.complete = true

		return nil
	}

	if f.Kind != PageFrame || f.Length == 0 || f.Offset < s.first || f.Offset >= s.end || f.Number != f.Offset/PageSize {
		return bad
	}

	start := max(s.first, f.Number*PageSize)

	end := min(s.end, (f.Number+1)*PageSize)
	if f.Offset != start || uint64(f.Length) != end-start || s.ordered && f.Number != s.first/PageSize+s.delivered {
		return bad
	}

	if !s.record(f.Number) {
		return bad
	}

	s.delivered++
	s.bytes += uint64(f.Length)

	return nil
}

// Write validates the same sequence as the decoder before encoding a frame header.
// The caller streams exactly Length payload bytes after each page header.
func (s *Sequence) Write(w io.Writer, f Frame) error {
	if err := s.Accept(f, "subscription frame"); err != nil {
		return err
	}

	return WriteFrame(w, f)
}

// record merges adjacent intervals with a hard memory limit for fragmented streams.
func (s *Sequence) record(n uint64) bool {
	i := 0
	for i < len(s.intervals) && s.intervals[i].end <= n {
		i++
	}

	if i < len(s.intervals) && s.intervals[i].first <= n {
		return false
	}

	if i > 0 && s.intervals[i-1].end == n {
		s.intervals[i-1].end++
		if i < len(s.intervals) && s.intervals[i].first == n+1 {
			s.intervals[i-1].end = s.intervals[i].end
			s.intervals = append(s.intervals[:i], s.intervals[i+1:]...)
		}

		return true
	}

	if i < len(s.intervals) && s.intervals[i].first == n+1 {
		s.intervals[i].first = n
		return true
	}

	if len(s.intervals) == 4096 {
		return false
	}

	s.intervals = append(s.intervals, pageInterval{})
	copy(s.intervals[i+1:], s.intervals[i:])
	s.intervals[i] = pageInterval{n, n + 1}

	return true
}
