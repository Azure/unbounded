// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestLiveTransferMetricsTrackBodyLifetimeAndSetupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "payload") }))
	defer server.Close()

	var active, liveBytes, legacyBytes atomic.Int64

	client, err := New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: server.URL}}},
		WithByteMetrics(func(_ string, n int64) { legacyBytes.Add(n) }),
		WithTransferMetrics(func(_ string, n int64) { liveBytes.Add(n) }, func(_ string, delta int) { active.Add(int64(delta)) }))
	if err != nil {
		t.Fatal(err)
	}

	ref := ifaces.OriginRef{Registry: "reg", Repository: "repo", Kind: ifaces.KindConfig}

	body, _, err := client.Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	if active.Load() != 1 {
		t.Fatal("origin body not counted as active")
	}

	if _, err := io.ReadFull(body, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}

	if liveBytes.Load() != 3 || legacyBytes.Load() != 0 || active.Load() != 1 {
		t.Fatal("live counters were delayed or changed legacy terminal reporting")
	}

	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}

	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	if active.Load() != 0 || liveBytes.Load() != 7 || legacyBytes.Load() != 7 {
		t.Fatal("origin transfer did not finish exactly once")
	}

	ref.Registry = "missing"
	if _, _, err := client.Pull(context.Background(), ref); err == nil {
		t.Fatal("unknown registry unexpectedly succeeded")
	}

	if active.Load() != 0 {
		t.Fatal("request setup failure leaked active origin gauge")
	}
}
