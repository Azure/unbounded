// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	godigest "github.com/opencontainers/go-digest"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestPeerTransferActivityIncludesOpenBodyAndClearsOnFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ns") == "busy" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		_, _ = io.WriteString(w, "payload")
	}))
	defer server.Close()

	var active, bytes atomic.Int64

	client := NewClient(WithClientTransferMetrics(func(_ string, n int64) { bytes.Add(n) }, func(_ string, delta int) { active.Add(int64(delta)) }))
	client.hc = server.Client()
	ref := ifaces.OriginRef{Registry: "reg", Repository: "repo", Digest: digest.MustParse(godigest.FromString("test").String()), Kind: ifaces.KindBlob}

	body, _, _, err := client.FetchFromPeer(context.Background(), strings.TrimPrefix(server.URL, "http://"), ref)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.ReadFull(body, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}

	if active.Load() != 1 || bytes.Load() != 3 {
		t.Fatal("peer body bytes were not reported while the transfer was active")
	}

	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	if active.Load() != 0 {
		t.Fatal("abandoned peer body remained active")
	}

	if _, _, _, err := client.FetchFromPeer(context.Background(), "invalid/peer", ref); err == nil {
		t.Fatal("invalid peer unexpectedly succeeded")
	}

	if active.Load() != 0 {
		t.Fatal("failed peer setup leaked active count")
	}
}
