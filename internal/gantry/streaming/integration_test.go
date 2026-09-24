// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // h2c is Gantry's peer protocol

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/streaming"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

func TestIntegrationTransitionsFromSignedOriginToCompletePeer(t *testing.T) {
	t.Parallel()

	complete := []byte("0123456789")
	d := mustStreamingDigest(t, complete)

	originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("origin Range = %q, want bytes=2-5", got)
		}

		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(complete[2:6]) //nolint:errcheck // best-effort test response
	}))
	t.Cleanup(originServer.Close)

	peerCache := fakes.NewCache()
	peerCache.Put(d, complete)
	peerAddr := startStreamingTransferServer(t, transfer.New(peerCache).Handler())

	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true}

	originClient, err := streaming.NewOriginClient(policy, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	discovery := &discoveryStub{}

	server, err := streaming.NewServer(
		&rangeStoreStub{err: &ifaces.ErrNotFound{Digest: d}},
		discovery,
		transfer.NewClient(transfer.WithRequestTimeout(5*time.Second)),
		originClient,
		streaming.Options{
			URLPolicy:         policy,
			PeerLookupTimeout: time.Second,
			MaxPeerAttempts:   2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	originURL := strings.Replace(originServer.URL, "127.0.0.1", "localhost", 1) +
		"?d=" + d.String() + "&sig=redacted"
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+originURL, nil)
		req.Header.Set("Range", "bytes=2-5")

		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)

		return response
	}

	first := request()
	if first.Code != http.StatusPartialContent || first.Body.String() != "2345" {
		t.Fatalf("origin response = %d %q, want 206 2345", first.Code, first.Body.String())
	}

	discovery.set(ifaces.Provider{NodeID: "peer-a", Addr: peerAddr})

	second := request()
	if second.Code != http.StatusPartialContent || second.Body.String() != "2345" {
		t.Fatalf("peer response = %d %q, want 206 2345", second.Code, second.Body.String())
	}

	if got := second.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("peer Content-Range = %q, want bytes 2-5/10", got)
	}
}

func startStreamingTransferServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	httpServer := &http.Server{
		Handler:           h2c.NewHandler(handler, &http2.Server{}), //nolint:staticcheck // h2c is Gantry's peer protocol
		ReadHeaderTimeout: time.Second,
	}

	go func() { _ = httpServer.Serve(listener) }() //nolint:errcheck // test cleanup owns shutdown

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_ = httpServer.Shutdown(ctx) //nolint:errcheck // best-effort test cleanup
	})

	return listener.Addr().String()
}

func mustStreamingDigest(t *testing.T, payload []byte) digest.Digest {
	t.Helper()

	d, err := digest.Parse("sha256:" + fmt.Sprintf("%x", sha256.Sum256(payload)))
	if err != nil {
		t.Fatal(err)
	}

	return d
}
