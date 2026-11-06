// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
)

func TestRangeTimeoutIsIndependentOfBulkTimeout(t *testing.T) {
	client := NewClient(WithRequestTimeout(15 * time.Minute))
	if client.rangeHC.Timeout != defaultRangeRequestTimeout || client.hc.Timeout != 15*time.Minute {
		t.Fatalf("range/bulk timeouts = %v/%v", client.rangeHC.Timeout, client.hc.Timeout)
	}

	if client.rangeHC.Transport == client.hc.Transport {
		t.Fatal("range and bulk requests share a transport")
	}

	addr := startHandlerOnEphemeral(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	client.rangeHC.Timeout = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	body, _, _, err := client.FetchRangeFromPeer(ctx, addr, d, httprange.Range{Start: 0, End: 3})
	if body != nil || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("body/error/caller = %v/%v/%v, want peer deadline without caller cancellation", body, err, ctx.Err())
	}
}
