// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/hex"
)

// InvalidateHead encodes `DELETE /v1/objects/{key}` naming the version to drop
// with a strong If-Match tag. The request has no body or origin context.
func InvalidateHead(key [32]byte, etag string) ([]byte, error) {
	if err := ValidateETag(etag); err != nil {
		return nil, err
	}

	return []byte("DELETE /v1/objects/" + hex.EncodeToString(key[:]) + " HTTP/1.1\r\nHost: racer\r\nIf-Match: " + etag + "\r\nContent-Length: 0\r\n\r\n"), nil
}

// ParseInvalidateResponse validates an invalidation response head: 204 is
// success; an error status carries no body or object metadata.
func ParseInvalidateResponse(head []byte) error {
	if err := ValidateRawHead(head, true); err != nil {
		return err
	}

	bad := failure(ErrorProtocol, "response", nil)

	h := HeadHeaders(head)
	if ForbiddenHeaders(h) || hasAnyHeader(h, "Etag", "Racer-Expires-At", "Racer-Content-Type", "Content-Range") {
		return bad
	}

	line := head[:bytes.Index(head, []byte("\r\n"))]
	if len(line) < 12 || !bytes.HasPrefix(line, []byte("HTTP/1.1 ")) || len(line) > 12 && line[12] != ' ' {
		return bad
	}

	status := 0

	for _, b := range line[9:12] {
		if b < '0' || b > '9' {
			return bad
		}

		status = status*10 + int(b-'0')
	}

	if status == 204 {
		// A 204 response has no body, so it must not claim one.
		if value := h.Get("Content-Length"); value != "" && value != "0" {
			return bad
		}

		return nil
	}

	length, err := Decimal(h.Get("Content-Length"))
	if err != nil || length != 0 {
		return bad
	}

	statusErr := StatusError(status)
	if statusErr.Kind == ErrorProtocol || status == 405 || status == 416 {
		return bad
	}

	return statusErr
}
