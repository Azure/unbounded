// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "canonical", value: "Bearer token", want: "Bearer token"},
		{name: "normalizes scheme", value: " bearer token ", want: "Bearer token"},
		{name: "basic", value: "Basic dXNlcjpwYXNz", want: "Basic dXNlcjpwYXNz"},
		{name: "normalizes basic scheme", value: " basic dXNlcjpwYXNz ", want: "Basic dXNlcjpwYXNz"},
		{name: "rejects malformed basic", value: "Basic not-base64"},
		{name: "rejects basic without colon", value: "Basic dXNlcg=="},
		{name: "rejects empty token", value: "Bearer "},
		{name: "rejects whitespace in token", value: "Bearer two tokens"},
		{name: "rejects unsupported scheme", value: "Digest token"},
		{name: "rejects oversized", value: "Bearer " + strings.Repeat("x", MaxAuthorizationBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.value); got != tt.want {
				t.Fatalf("Normalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetachRetainsOnlyAuthorization(t *testing.T) {
	type otherKey struct{}

	ctx := context.WithValue(context.Background(), otherKey{}, "other")
	ctx = WithAuthorization(ctx, "Bearer delegated")
	detached := Detach(ctx)

	if got := Authorization(detached); got != "Bearer delegated" {
		t.Fatalf("Authorization() = %q, want delegated bearer", got)
	}

	if got := detached.Value(otherKey{}); got != nil {
		t.Fatalf("unrelated context value retained: %v", got)
	}
}

func TestWithAnonymous(t *testing.T) {
	if Anonymous(context.Background()) {
		t.Fatal("background context is anonymous")
	}

	ctx := WithAnonymous(WithAuthorization(context.Background(), "Bearer tok"))
	if !Anonymous(ctx) {
		t.Fatal("WithAnonymous not reported")
	}

	if got := Authorization(ctx); got != "" {
		t.Fatalf("delegated authorization survived: %q", got)
	}

	if Anonymous(nil) { //nolint:staticcheck // nil context is handled explicitly
		t.Fatal("nil context is anonymous")
	}
}
