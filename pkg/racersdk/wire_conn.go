// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// readRawHead consumes only one head from a connection-owned buffered reader.
// Keep using that reader for the body and next head: it may have read ahead.
// Memory is capped independently of line lengths. The caller owns deadlines,
// cancellation, sequential framing, and connection closure on ANY head error.
// Do not pool the returned bytes (they can contain upstream credentials).
func readRawHead(r *bufio.Reader, response bool) ([]byte, error) {
	head := make([]byte, 0, 1024)
	for len(head) < maxHeadBytes {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && len(head) != 0 {
				err = io.ErrUnexpectedEOF
			}

			return nil, ioFailure("head read", err)
		}

		head = append(head, b)
		if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
			if err := validateRawHead(head, response); err != nil {
				return nil, err
			}

			return head, nil
		}
	}

	return nil, headFailure(response, true)
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
func validateRawHead(head []byte, response bool) error {
	if len(head) > maxHeadBytes {
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
		if name == "racer-metadata" || name == "authorization" {
			if len(value) == 0 || value[0] != ' ' {
				return headFailure(response, false)
			}

			if err := validateOpaque(string(value[1:])); err != nil {
				return headFailure(response, len(value)-1 > maxFieldBytes)
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
	case "host", "content-length", "content-type", "content-range", "etag", "if-match", "range", "racer-expires-at", "racer-metadata", "authorization":
		return true
	default:
		return false
	}
}

// headHeaders preserves values after standard HTTP OWS normalization. Only call
// after validateRawHead; opaque context has already been verified byte-exact.
func headHeaders(head []byte) http.Header {
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
type frameReader struct {
	source    io.Reader
	remaining int64
}

func (r *frameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if r.remaining == 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}

	n, err := r.source.Read(p)

	r.remaining -= int64(n)
	if err == io.EOF {
		if r.remaining != 0 {
			err = io.ErrUnexpectedEOF
		} else {
			err = nil
		}
	}

	return n, ioFailure("body read", err)
}
