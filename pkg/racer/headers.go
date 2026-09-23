// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const MaxAuthorizationBytes = 64 << 10

func validAuthorization(value string) bool {
	if value == "" || len(value) > MaxAuthorizationBytes || strings.TrimSpace(value) != value {
		return false
	}

	for i := range value {
		if value[i] < 32 || value[i] > 126 {
			return false
		}
	}

	return true
}

// WithAuthorization returns an immutable view sharing both connection pools.
// Empty removes Authorization; a nonempty value must satisfy Racer's opaque
// ASCII credential contract. Errors never include the credential.
func (c *Client) WithAuthorization(value string) (*Client, error) {
	if value != "" && !validAuthorization(value) {
		return nil, fmt.Errorf("racer: invalid Authorization")
	}

	view := *c
	view.header = c.header.Clone()
	view.header.Del("Authorization")

	if value != "" {
		view.header.Set("Authorization", value)
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

type authorizationKey struct{}

// AuthorizationFromContext returns the validated request credential supplied by
// Origin. An empty string means absent. Stores must not retain or log it.
func AuthorizationFromContext(ctx context.Context) string {
	value, ok := ctx.Value(authorizationKey{}).(string)
	if !ok {
		return ""
	}

	return value
}
