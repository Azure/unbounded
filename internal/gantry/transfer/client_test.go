// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // h2c deliberate

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// startTransferOnEphemeral starts an h2c transfer server on an ephemeral
// loopback port and returns "host:port". Cleanup is registered with t.
func startTransferOnEphemeral(t *testing.T, cache ifaces.LocalContentStore) string {
	t.Helper()

	srv := New(cache)

	return startHandlerOnEphemeral(t, srv.Handler())
}

func startHandlerOnEphemeral(t *testing.T, handler http.Handler) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h2s := &http2.Server{}
	handler = h2c.NewHandler(handler, h2s) //nolint:staticcheck // h2c deliberate

	hsrv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		_ = hsrv.Serve(ln) //nolint:errcheck // best-effort
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = hsrv.Shutdown(ctx) //nolint:errcheck // best-effort
		_ = ln.Close()         //nolint:errcheck // best-effort close
	})

	return ln.Addr().String()
}

func TestClientDefaultRequestTimeout(t *testing.T) {
	client := NewClient()

	if client.hc.Timeout != time.Hour {
		t.Fatalf("request timeout = %v, want 1h", client.hc.Timeout)
	}
}

func TestClientAdvertisesLargePeerFrames(t *testing.T) {
	client := NewClient()

	transport, ok := client.hc.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http2.Transport", client.hc.Transport)
	}

	if transport.MaxReadFrameSize != peerMaxReadFrameSize {
		t.Fatalf("MaxReadFrameSize = %d, want %d", transport.MaxReadFrameSize, peerMaxReadFrameSize)
	}
}

func TestClientForwardsDelegatedAuthorization(t *testing.T) {
	for _, authorization := range []string{
		"Bearer requester-token",
		"Basic cmVxdWVzdGVyOnNlY3JldA==",
	} {
		t.Run(strings.Fields(authorization)[0], func(t *testing.T) {
			body := []byte("peer-served bytes")
			d := mustDigest(body)

			addr := startHandlerOnEphemeral(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != authorization {
					t.Errorf("Authorization = %q, want %q", got, authorization)
				}

				_, _ = w.Write(body) //nolint:errcheck // best-effort write
			}))

			ctx := registryauth.WithAuthorization(context.Background(), authorization)

			rc, _, _, err := NewClient().FetchFromPeer(ctx, addr, ifaces.OriginRef{Repository: "repo", Digest: d})
			if err != nil {
				t.Fatalf("FetchFromPeer: %v", err)
			}

			_ = rc.Close() //nolint:errcheck // best-effort close
		})
	}
}

func TestClientFetchOK(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte("peer-served bytes")
	d := mustDigest(body)
	cache.Put(d, body)

	addr := startTransferOnEphemeral(t, cache)
	client := NewClient(WithDialTimeout(time.Second), WithRequestTimeout(5*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc, size, contentType, err := client.FetchFromPeer(ctx, addr, ifaces.OriginRef{
		Repository: "myrepo",
		Digest:     d,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer: %v", err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}

	if contentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", contentType)
	}

	got, _ := io.ReadAll(rc)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)

	if got := parseRetryAfter("3", now); got != 3*time.Second {
		t.Fatalf("delay-seconds Retry-After = %v, want 3s", got)
	}

	if got := parseRetryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now); got != 5*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, want 5s", got)
	}

	for _, value := range []string{"", "invalid", "-1", now.Add(-time.Second).Format(http.TimeFormat)} {
		if got := parseRetryAfter(value, now); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", value, got)
		}
	}
}

func TestClientFetchBusyPreservesRetryAfter(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &http2.Server{}

	t.Cleanup(func() { _ = listener.Close() })

	// Use the package's existing ephemeral transfer fixture pattern through a
	// minimal h2c handler that returns only the capacity signal under test.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "busy", http.StatusTooManyRequests)
	})

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go server.ServeConn(conn, &http2.ServeConnOpts{Handler: handler})
		}
	}()

	_, _, _, err = NewClient(WithRequestTimeout(time.Second)).FetchFromPeer(context.Background(), listener.Addr().String(), ifaces.OriginRef{
		Repository: "repo",
		Digest:     mustDigest([]byte("busy")),
	})

	var statusErr *ifaces.ErrPeerHTTPStatus
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T %v, want ErrPeerHTTPStatus", err, err)
	}

	if statusErr.StatusCode != http.StatusTooManyRequests || statusErr.RetryAfter != 3*time.Second {
		t.Fatalf("status error = %+v, want 429 with 3s Retry-After", statusErr)
	}
}

func TestClientFetchRange(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte("peer-served bytes")
	d := mustDigest(body)
	cache.Put(d, body)

	addr := startTransferOnEphemeral(t, cache)

	rc, size, _, err := NewClient().FetchFromPeer(context.Background(), addr, ifaces.OriginRef{
		Repository: "myrepo",
		Digest:     d,
		Offset:     5,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer: %v", err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	if size != int64(len(body)) {
		t.Fatalf("size = %d, want full size %d", size, len(body))
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != string(body[5:]) {
		t.Fatalf("body = %q, want %q", got, body[5:])
	}
}

func TestClientRejectsInvalidRangeResponse(t *testing.T) {
	body := []byte("wrong range")
	d := mustDigest(body)
	addr := startHandlerOnEphemeral(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-10/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))

	rc, _, _, err := NewClient().FetchFromPeer(context.Background(), addr, ifaces.OriginRef{
		Repository: "myrepo",
		Digest:     d,
		Offset:     5,
	})
	if rc != nil {
		_ = rc.Close() //nolint:errcheck // best-effort close
	}

	if err == nil || !strings.Contains(err.Error(), "invalid Content-Range") {
		t.Fatalf("error = %v, want invalid Content-Range", err)
	}
}

func TestClientByteMetricsReportsPartialReadOnClose(t *testing.T) {
	body := []byte("peer-served bytes with a deliberately partial read")
	d := mustDigest(body)

	cache := fakes.NewCache()
	cache.Put(d, body)
	addr := startTransferOnEphemeral(t, cache)

	var observations []int64

	client := NewClient(
		WithDialTimeout(time.Second),
		WithRequestTimeout(5*time.Second),
		WithClientByteMetrics(func(kind string, bytes int64) {
			if kind != "layer" {
				t.Errorf("kind = %q, want layer", kind)
			}

			observations = append(observations, bytes)
		}),
	)

	rc, _, _, err := client.FetchFromPeer(context.Background(), addr, ifaces.OriginRef{
		Repository: "myrepo",
		Digest:     d,
		Kind:       ifaces.KindBlob,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer: %v", err)
	}

	buf := make([]byte, 5)

	if n, err := rc.Read(buf); err != nil || n != len(buf) {
		t.Fatalf("Read = (%d, %v), want (%d, nil)", n, err, len(buf))
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_ = rc.Close() //nolint:errcheck // verify no duplicate observation

	if len(observations) != 1 || observations[0] != int64(len(buf)) {
		t.Fatalf("observations = %v, want [%d]", observations, len(buf))
	}
}

func TestClientFetchNotFound(t *testing.T) {
	cache := fakes.NewCache()
	addr := startTransferOnEphemeral(t, cache)
	client := NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + strings.Repeat("d", 64))

	_, _, _, err := client.FetchFromPeer(ctx, addr, ifaces.OriginRef{
		Repository: "r",
		Digest:     d,
	})
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}

	var enf *ifaces.ErrNotFound
	if !errors.As(err, &enf) {
		t.Errorf("error = %T %v, want *ErrNotFound", err, err)
	}
}

func TestClientFetchUnauthorizedStatus(t *testing.T) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h2s := &http2.Server{}

	hsrv := &http.Server{
		Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:staticcheck // h2c deliberate
			w.WriteHeader(http.StatusUnauthorized)
		}), h2s),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = hsrv.Serve(ln) //nolint:errcheck // best-effort
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = hsrv.Shutdown(ctx) //nolint:errcheck // best-effort
		_ = ln.Close()         //nolint:errcheck // best-effort close
	})

	client := NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + strings.Repeat("e", 64))

	_, _, _, err = client.FetchFromPeer(ctx, ln.Addr().String(), ifaces.OriginRef{Repository: "r", Digest: d})
	if err == nil {
		t.Fatal("expected status error, got nil")
	}

	var statusErr *ifaces.ErrPeerHTTPStatus
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T %v, want *ErrPeerHTTPStatus", err, err)
	}

	if statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", statusErr.StatusCode, http.StatusUnauthorized)
	}
}

func TestClientManifestPath(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte(`{"schemaVersion":2}`)
	d := mustDigest(body)
	cache.Put(d, body)

	addr := startTransferOnEphemeral(t, cache)
	client := NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc, _, _, err := client.FetchFromPeer(ctx, addr, ifaces.OriginRef{
		Repository: "r",
		Digest:     d,
		Kind:       ifaces.KindManifest,
	})
	if err != nil {
		t.Fatalf("FetchFromPeer manifest: %v", err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	got, _ := io.ReadAll(rc)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestClientDialFailure(t *testing.T) {
	client := NewClient(WithDialTimeout(200 * time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	// Port 1 is unreachable.
	_, _, _, err := client.FetchFromPeer(ctx, "127.0.0.1:1", ifaces.OriginRef{
		Repository: "r",
		Digest:     d,
	})
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}

	var enf *ifaces.ErrNotFound
	if errors.As(err, &enf) {
		t.Errorf("dial failure surfaced as ErrNotFound; should remain a transport error: %v", err)
	}
}

func TestBuildPeerURL(t *testing.T) {
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

	cases := []struct {
		name string
		addr string
		ref  ifaces.OriginRef
		want string
		err  bool
	}{
		{
			name: "blob",
			addr: "10.0.0.1:5001",
			ref:  ifaces.OriginRef{Repository: "library/nginx", Digest: d},
			want: "http://10.0.0.1:5001/v2/library/nginx/blobs/" + d.String(),
		},
		{
			name: "manifest",
			addr: "10.0.0.1:5001",
			ref:  ifaces.OriginRef{Repository: "library/nginx", Digest: d, Kind: ifaces.KindManifest},
			want: "http://10.0.0.1:5001/v2/library/nginx/manifests/" + d.String(),
		},
		{
			name: "missing-addr",
			addr: "",
			ref:  ifaces.OriginRef{Repository: "r", Digest: d},
			err:  true,
		},
		{
			name: "missing-repo",
			addr: "10.0.0.1:5001",
			ref:  ifaces.OriginRef{Digest: d},
			want: "http://10.0.0.1:5001/v2/gantry/blobs/" + d.String(),
		},
		{
			name: "invalid-repo",
			addr: "10.0.0.1:5001",
			ref:  ifaces.OriginRef{Repository: "../../etc", Digest: d},
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildPeerURL(tc.addr, tc.ref)
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}

				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
