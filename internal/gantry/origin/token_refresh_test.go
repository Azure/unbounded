// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTokenRefreshWaitCancellationAndIsolation(t *testing.T) {
	flight := &tokenRefresh{challenge: "scope-a", done: make(chan struct{}), token: "a"}
	r := &registry{tokenRefresh: flight}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.refreshBearerToken(ctx, "scope-a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	close(flight.done)

	if token, err := r.refreshBearerToken(context.Background(), "scope-a"); err != nil || token != "a" {
		t.Fatal(token, err)
	}

	r.tokenRefresh = nil
	if _, err := r.refreshBearerToken(context.Background(), "invalid"); err == nil {
		t.Fatal("invalid challenge accepted")
	}

	if r.tokenRefresh != nil {
		t.Fatal("failed refresh retained")
	}

	r.setToken("new", time.Hour)
	r.clearToken("old")

	if r.cachedToken() != "new" {
		t.Fatal("late rejection cleared refreshed token")
	}
}
