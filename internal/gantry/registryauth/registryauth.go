// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package registryauth carries short-lived registry authorization between
// Gantry's mirror, peer, coordination, and origin request boundaries.
package registryauth

import (
	"context"
	"encoding/base64"
	"strings"
)

// MaxAuthorizationBytes bounds delegated credentials accepted from containerd
// or another Gantry peer. Registry credentials are normally a few KiB.
const MaxAuthorizationBytes = 64 * 1024

type contextKey struct{}

// Normalize validates and normalizes an Authorization header value for
// delegation. Basic and Bearer are supported; other schemes are rejected.
// The returned value is empty for an absent, malformed, unsupported, or
// oversized header.
func Normalize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > MaxAuthorizationBytes {
		return ""
	}

	scheme, credential, ok := strings.Cut(value, " ")
	if !ok {
		return ""
	}

	credential = strings.TrimSpace(credential)
	if credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return ""
	}

	switch {
	case strings.EqualFold(scheme, "Bearer"):
		return "Bearer " + credential
	case strings.EqualFold(scheme, "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(credential)
		if err != nil || !strings.ContainsRune(string(decoded), ':') {
			return ""
		}

		return "Basic " + credential
	default:
		return ""
	}
}

// WithAuthorization returns a child context carrying a normalized registry
// authorization value. Empty or invalid values leave ctx unchanged.
func WithAuthorization(ctx context.Context, authorization string) context.Context {
	authorization = Normalize(authorization)
	if authorization == "" {
		return ctx
	}

	return context.WithValue(ctx, contextKey{}, authorization)
}

// Authorization returns the delegated registry Authorization header in ctx.
func Authorization(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	authorization, ok := ctx.Value(contextKey{}).(string)
	if !ok {
		return ""
	}

	return authorization
}

// Detach returns a background context containing only the delegated registry
// authorization. It is used for bounded work that must outlive the inbound
// mirror or coordination request without retaining unrelated request values.
func Detach(ctx context.Context) context.Context {
	return WithAuthorization(context.Background(), Authorization(ctx))
}
