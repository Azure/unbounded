// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// MaxOriginDataBytes is the maximum decoded request-scoped origin data size.
const MaxOriginDataBytes = 64 << 10

const maxEncodedOriginDataBytes = (MaxOriginDataBytes + 2) / 3 * 4

// WithOriginData returns an immutable view sharing both connection pools and
// the client-wide active request limit.
// It copies arbitrary bytes; nil or empty removes origin data. Origin data is
// forwarded on cache misses, not included in persistent object identity, and
// must not be used for per-read authorization. Errors never include the data.
func (c *Client) WithOriginData(value []byte) (*Client, error) {
	if len(value) > MaxOriginDataBytes {
		return nil, fmt.Errorf("racer: origin data too large")
	}

	view := *c
	view.header = c.header.Clone()
	view.header.Del("Racer-Origin-Data")

	if len(value) != 0 {
		view.header.Set("Racer-Origin-Data", base64.StdEncoding.EncodeToString(value))
	}

	return &view, nil
}

func validField(value string, limit int) bool {
	if len(value) > limit {
		return false
	}

	for i := range value {
		if value[i] == 127 || value[i] < 32 && value[i] != '\t' {
			return false
		}
	}

	return true
}

func boundedField(h http.Header, name string, limit int) (string, error) {
	values := h.Values(name)
	if len(values) == 0 {
		return "", nil
	}

	value := strings.Trim(values[0], " \t")
	if len(values) != 1 || value == "" || !validField(value, limit) {
		return "", fmt.Errorf("%w: invalid %s", ErrProtocol, name)
	}

	return value, nil
}

func responseError(method, target string, resp *http.Response) error {
	challenge, err := boundedField(resp.Header, "WWW-Authenticate", 1024)
	if err != nil {
		return err
	}

	retry, err := boundedField(resp.Header, "Retry-After", 128)
	if err != nil {
		return err
	}

	return &HTTPError{Method: method, Target: target, StatusCode: resp.StatusCode, WWWAuthenticate: challenge, RetryAfter: retry}
}

func decodeOriginData(h http.Header) ([]byte, int) {
	values := h.Values("Racer-Origin-Data")
	if len(values) == 0 {
		return nil, 0
	}

	if len(values) != 1 {
		return nil, http.StatusBadRequest
	}

	if len(values[0]) > maxEncodedOriginDataBytes {
		return nil, http.StatusRequestHeaderFieldsTooLarge
	}

	data, err := base64.StdEncoding.Strict().DecodeString(values[0])
	if err != nil || len(data) > MaxOriginDataBytes || base64.StdEncoding.EncodeToString(data) != values[0] {
		return nil, http.StatusBadRequest
	}

	return data, 0
}
