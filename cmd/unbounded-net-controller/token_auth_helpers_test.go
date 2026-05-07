// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTokenAuthenticatorHeaderGuardsAndNilIDToken tests TokenAuthenticatorHeaderGuards.
func TestTokenAuthenticatorHeaderGuardsAndNilIDToken(t *testing.T) {
	auth := &tokenAuthenticator{
		cache:          make(map[string]*tokenAuthResult),
		cacheTTL:       time.Minute,
		allowedSANames: map[string]bool{},
	}

	reqNoHeader := httptest.NewRequest(http.MethodGet, "/", nil)
	if auth.authenticate(reqNoHeader) {
		t.Fatalf("expected authenticate=false when Authorization header is missing")
	}

	reqWrongScheme := httptest.NewRequest(http.MethodGet, "/", nil)
	reqWrongScheme.Header.Set("Authorization", "Basic abc123")

	if auth.authenticate(reqWrongScheme) {
		t.Fatalf("expected authenticate=false when Authorization scheme is not Bearer")
	}
}
