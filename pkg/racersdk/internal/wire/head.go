// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package wire implements Racer HTTP heads, metadata grammar, and subscription framing.
// It owns no connections, deadlines, admission, or payload storage.
package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	MaxHeadBytes  = 32 * 1024
	MaxFieldBytes = 8192
)

// ErrorKind is a protocol classification, explicitly translated by the SDK boundary.
type ErrorKind uint8

const (
	ErrorInvalidArgument ErrorKind = iota + 1
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

// readRawHead consumes only one head from a connection-owned buffered reader.
// Keep using that reader for the body and next head: it may have read ahead.
// Memory is capped independently of line lengths. The caller owns deadlines,
// cancellation, sequential framing, and connection closure on ANY head error.
// Do not pool the returned bytes (they can contain upstream credentials).
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

// readHeadBytes only finds the bounded frame; semantic parsers validate it once.
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

// validateRawHead MUST run before net/http parsing. Header.Get, Values, and
// parsed Request/Response objects cannot recover trimmed context whitespace or
// duplicate Content-Length fields that the stdlib has already coalesced.
// This checks raw grammar/limits, not operation semantics; use parseRequestHead
// or parseResponseHead as well. Unknown fields still count toward the bound.
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

// headHeaders preserves values after standard HTTP OWS normalization. Only call
// after validateRawHead; opaque context has already been verified byte-exact.
func HeadHeaders(head []byte) http.Header {
	h := make(http.Header)

	lines := bytes.Split(head[:len(head)-4], []byte("\r\n"))
	for _, line := range lines[1:] {
		i := bytes.IndexByte(line, ':')
		h.Add(string(line[:i]), strings.Trim(string(line[i+1:]), " \t"))
	}

	return h
}

// frameReader is a fixed-length streaming boundary over the SAME buffered
// reader used by readRawHead. It never probes past the HTTP frame and never
// closes its input. Zero length is immediately EOF. A truncated frame is a typed
// I/O error wrapping io.ErrUnexpectedEOF. Other errors with final bytes survive.
// The caller must validate length first and must not reuse a failed/aborted frame.
// Callback bodies need the stronger final-byte holdback/EOF probe in the server;
// this HTTP framing helper intentionally cannot prove absence of extra bytes.
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
