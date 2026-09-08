// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
)

// fakePeerSource implements PeerSource for unit tests.
type fakePeerSource struct {
	mu    func() ([]Peer, error)
	calls atomic.Int64
}

func (f *fakePeerSource) Peers(_ context.Context) ([]Peer, error) {
	f.calls.Add(1)

	return f.mu()
}

// TestRefresh_RetainsPreviousOnError verifies that a discovery error
// after a successful refresh retains the previous peer-set rather
// than clobbering it with [Self].
//
// Regression test for B3.
func TestRefresh_RetainsPreviousOnError(t *testing.T) {
	t.Parallel()

	good := []Peer{
		{IP: "10.0.0.1", Self: false},
		{IP: "10.0.0.2", Self: true},
		{IP: "10.0.0.3", Self: false},
	}

	var failing atomic.Bool

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			if failing.Load() {
				return nil, errors.New("transient DNS failure")
			}

			out := make([]Peer, len(good))
			copy(out, good)

			return out, nil
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.2",
			MembershipRefresh: time.Hour, // disable auto-refresh; we drive it manually
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	// Initial refresh ran during New; verify good peers are loaded.
	if got := len(c.Peers()); got != 3 {
		t.Fatalf("initial Peers()=%d want 3", got)
	}

	failing.Store(true)
	// First few error refreshes: retain previous snapshot.
	for i := 0; i < maxStalePeerRefreshes; i++ {
		c.refresh(t.Context())

		if got := len(c.Peers()); got != 3 {
			t.Errorf("after error %d: Peers()=%d want 3 (retain previous)", i+1, got)
		}
	}
	// Next refresh exceeds the staleness ceiling -> fall back to self.
	c.refresh(t.Context())

	if got := c.Peers(); len(got) != 1 || !got[0].Self {
		t.Errorf("after ceiling exceeded: Peers()=%+v want [Self]", got)
	}
	// Recovery: source returns good peers again. Error counter resets.
	failing.Store(false)
	c.refresh(t.Context())

	if got := len(c.Peers()); got != 3 {
		t.Errorf("after recovery: Peers()=%d want 3", got)
	}

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Errorf("error counter not reset after success: got %d", got)
	}
}

// TestRefresh_BootstrapErrorFallsBackToSelf verifies that on bootstrap
// (no previous snapshot) a discovery error falls back to [Self]
// immediately - we cannot retain something that does not exist.
func TestRefresh_BootstrapErrorFallsBackToSelf(t *testing.T) {
	t.Parallel()

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			return nil, errors.New("DNS not reachable yet")
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	got := c.Peers()
	if len(got) != 1 || !got[0].Self {
		t.Errorf("bootstrap with error source: Peers()=%+v want [Self]", got)
	}
}

// TestRefresh_EmptyResultFallsBackToSelf verifies that a successful
// discovery returning zero peers (the legitimate "I'm alone" answer)
// still falls back to [Self] without bumping the error counter.
func TestRefresh_EmptyResultFallsBackToSelf(t *testing.T) {
	t.Parallel()

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			return nil, nil // no error, zero peers
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	got := c.Peers()
	if len(got) != 1 || !got[0].Self {
		t.Errorf("empty source: Peers()=%+v want [Self]", got)
	}

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Errorf("empty (non-error) result should not bump error counter; got %d", got)
	}
}

// TestFillFromPeer_DetectsTruncation verifies that the validating
// reader returned by FillFromPeer surfaces io.ErrUnexpectedEOF when
// the peer advertises a Content-Length but the connection closes
// before that many bytes have been delivered. Without the validator
// the requester would observe a clean io.EOF and silently pass
// short bytes through to the client.
//
// Regression test for B7.
func TestFillFromPeer_DetectsTruncation(t *testing.T) {
	t.Parallel()

	const advertised = 100

	const delivered = 50

	// Use a raw TCP listener so we have full control over the wire
	// format: write Content-Length: 100, then write 50 body bytes,
	// then close the connection mid-stream.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() }) //nolint:errcheck // test cleanup

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		defer conn.Close() //nolint:errcheck // test cleanup
		// Consume request headers up through the blank line.
		buf := make([]byte, 4096)

		if _, err := conn.Read(buf); err != nil {
			return
		}

		resp := "HTTP/1.1 200 OK\r\n" +
			"Content-Length: " + strconv.Itoa(advertised) + "\r\n" +
			"Content-Type: application/octet-stream\r\n" +
			"\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}

		if _, err := conn.Write(make([]byte, delivered)); err != nil {
			return
		}
		// Close mid-body without writing the remaining bytes.
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
			InternalListen:    "0.0.0.0:8444",
		},
		WithPeerSource(&fakePeerSource{mu: func() ([]Peer, error) {
			return []Peer{{IP: "10.0.0.1", Self: true}}, nil
		}}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	peer := Peer{IP: host, Port: port}
	key := chunk.Key{
		OriginID:  "test-origin",
		Bucket:    "test-bucket",
		ObjectKey: "test-object",
		ETag:      "test-etag",
		ChunkSize: advertised,
		Index:     0,
	}

	body, err := c.FillFromPeer(t.Context(), peer, key, advertised)
	if err != nil {
		t.Fatalf("FillFromPeer: %v", err)
	}

	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got err=%v (read %d bytes)", err, len(got))
	}

	if len(got) != delivered {
		t.Errorf("got %d bytes, expected %d (the delivered prefix)", len(got), delivered)
	}
}

// TestNewHTTPClient_NoWallTimeout asserts that the default
// internal-RPC HTTP client carries no Client.Timeout. Client.Timeout
// is a request-total wall clock that would clamp long-running fill
// body streams (an 8 MiB chunk on a degraded inter-pod link can
// exceed any reasonable hardcoded bound). The caller's ctx is the
// sole deadline for body reads.
func TestNewHTTPClient_NoWallTimeout(t *testing.T) {
	t.Parallel()

	c, err := newHTTPClient(config.Cluster{})
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	if c.Timeout != 0 {
		t.Errorf("internal-RPC http.Client.Timeout = %v, want 0", c.Timeout)
	}
}

// TestNewHTTPClient_ConnectTimeouts asserts that the Transport
// carries bounded connect-level timeouts independent of the
// caller's ctx. Without these, a stuck TCP SYN or stalled TLS
// handshake against a half-failed peer would hang until the
// caller's deadline (which is the full 5-minute fill ctx for
// leader-side fills, causing slot starvation).
//
// Regression for H-4.
func TestNewHTTPClient_ConnectTimeouts(t *testing.T) {
	t.Parallel()

	c, err := newHTTPClient(config.Cluster{})
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T; want *http.Transport", c.Transport)
	}

	if tr.TLSHandshakeTimeout == 0 {
		t.Errorf("TLSHandshakeTimeout is 0; want bounded")
	}

	if tr.DialContext == nil {
		t.Errorf("DialContext is nil; expected bounded dialer")
	}
}

// TestNewHTTPClient_InternalTLSEnabledRefusesToStart verifies that
// newHTTPClient refuses to construct a client when
// cfg.InternalTLS.Enabled=true. The TLS configuration is not yet
// wired into the transport (no TLSClientConfig); returning a working
// client in that case would silently dial https:// against the
// system trust store instead of the configured CA, downgrading the
// security posture. The constructor must fail loudly until the
// production TLS wiring is implemented.
func TestNewHTTPClient_InternalTLSEnabledRefusesToStart(t *testing.T) {
	t.Parallel()

	cfg := config.Cluster{
		InternalTLS: config.InternalTLS{Enabled: true},
	}

	c, err := newHTTPClient(cfg)
	if err == nil {
		t.Fatalf("newHTTPClient with InternalTLS.Enabled=true returned client %v; want error", c)
	}
}

// TestFillFromPeer_CtxDeadlineHonored verifies that the caller's ctx
// deadline (rather than any hardcoded wall clock inside the cluster's
// HTTP client) is what bounds the cross-replica fill. Sets up a
// slow-paced TCP server that delivers a full Content-Length body
// over ~250ms, and calls FillFromPeer with a 50ms ctx; expects the
// read to fail with context.DeadlineExceeded.
//
// Companion to the wall-timeout removal: regression-tests that ctx
// propagation still bounds the request even though the
// Client.Timeout safety net is gone.
func TestFillFromPeer_CtxDeadlineHonored(t *testing.T) {
	t.Parallel()

	const advertised = 1024

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() }) //nolint:errcheck // test cleanup

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		defer conn.Close() //nolint:errcheck // test cleanup

		buf := make([]byte, 4096)
		if _, err := conn.Read(buf); err != nil {
			return
		}

		resp := "HTTP/1.1 200 OK\r\n" +
			"Content-Length: " + strconv.Itoa(advertised) + "\r\n" +
			"Content-Type: application/octet-stream\r\n" +
			"\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		// Drip body bytes slowly: 64 bytes every 20ms (~ 320ms for
		// the full 1 KiB), far exceeding the 50ms ctx deadline.
		body := make([]byte, advertised)

		for i := 0; i < advertised; i += 64 {
			end := i + 64
			if end > advertised {
				end = advertised
			}

			if _, err := conn.Write(body[i:end]); err != nil {
				return
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
			InternalListen:    "0.0.0.0:8444",
		},
		WithPeerSource(&fakePeerSource{mu: func() ([]Peer, error) {
			return []Peer{{IP: "10.0.0.1", Self: true}}, nil
		}}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	peer := Peer{IP: host, Port: port}
	key := chunk.Key{
		OriginID:  "test-origin",
		Bucket:    "test-bucket",
		ObjectKey: "test-object",
		ETag:      "test-etag",
		ChunkSize: advertised,
		Index:     0,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	body, err := c.FillFromPeer(ctx, peer, key, advertised)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FillFromPeer err = %v, want context.DeadlineExceeded (or success then deadline on read)", err)
		}

		return
	}

	defer body.Close() //nolint:errcheck // test cleanup

	_, readErr := io.ReadAll(body)
	if !errors.Is(readErr, context.DeadlineExceeded) {
		t.Errorf("ReadAll err = %v, want context.DeadlineExceeded", readErr)
	}
}

// TestWithHTTPClient_Overrides verifies the test seam: tests can
// inject an alternate http.Client (used to give a deterministic
// short timeout or custom transport behavior).
func TestWithHTTPClient_Overrides(t *testing.T) {
	t.Parallel()

	custom := &http.Client{Timeout: 42 * time.Millisecond}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(&fakePeerSource{mu: func() ([]Peer, error) {
			return []Peer{{IP: "10.0.0.1", Self: true}}, nil
		}}),
		WithHTTPClient(custom),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	if c.httpClient != custom {
		t.Errorf("httpClient not overridden by WithHTTPClient")
	}
}

// TestWithLogger_OverridesDefault verifies the cluster honors the
// injected slog.Logger so cluster.refresh's warn-level
// retain-snapshot message and the debug-level emissions route to
// the caller's configured handler rather than slog.Default.
func TestWithLogger_OverridesDefault(t *testing.T) {
	t.Parallel()

	injected := slog.New(slog.NewTextHandler(io.Discard, nil))

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(&fakePeerSource{mu: func() ([]Peer, error) {
			return []Peer{{IP: "10.0.0.1", Self: true}}, nil
		}}),
		WithLogger(injected),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	if c.log != injected {
		t.Errorf("Cluster.log not the injected logger")
	}
}

// TestRefresh_EmitsMembershipTransition verifies that a peer-set
// change (member added) surfaces a Info-level 'peer_set_changed'
// log line. Stable refreshes (no delta) must not re-emit this line.
func TestRefresh_EmitsMembershipTransition(t *testing.T) {
	t.Parallel()

	initial := []Peer{
		{IP: "10.0.0.2", Self: true},
	}

	current := initial

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			out := make([]Peer, len(current))
			copy(out, current)

			return out, nil
		},
	}

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.2",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(src),
		WithLogger(log),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	// Initial snapshot landed during New: peer_set_initial emitted.
	if !strings.Contains(buf.String(), "peer_set_initial") {
		t.Errorf("expected peer_set_initial on bootstrap; got %q", buf.String())
	}

	buf.Reset()

	// Stable refresh: no delta -> only the debug peer_set_refreshed.
	c.refresh(t.Context())

	if strings.Contains(buf.String(), "peer_set_changed") {
		t.Errorf("peer_set_changed should not fire when peer-set is stable; got %q", buf.String())
	}

	if !strings.Contains(buf.String(), "peer_set_refreshed") {
		t.Errorf("expected per-cycle peer_set_refreshed; got %q", buf.String())
	}

	buf.Reset()

	// Add a peer: peer_set_changed must fire with the 'added' key.
	current = append([]Peer{}, initial...)
	current = append(current, Peer{IP: "10.0.0.3"})

	c.refresh(t.Context())

	if !strings.Contains(buf.String(), "peer_set_changed") {
		t.Errorf("peer_set_changed missing on add; got %q", buf.String())
	}

	if !strings.Contains(buf.String(), "10.0.0.3") {
		t.Errorf("added peer IP missing from log; got %q", buf.String())
	}
}

// TestCoordinator_EmitsDebugSelection verifies the per-call debug
// emission carrying the chosen-peer and rendezvous score for a
// chunk. Operators rely on this to diagnose routing surprises.
func TestCoordinator_EmitsDebugSelection(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.1",
			MembershipRefresh: time.Hour,
		},
		WithPeerSource(&fakePeerSource{mu: func() ([]Peer, error) {
			return []Peer{{IP: "10.0.0.1", Self: true}}, nil
		}}),
		WithLogger(log),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	buf.Reset()

	c.Coordinator(chunk.Key{
		OriginID: "ox", Bucket: "b", ObjectKey: "o", ChunkSize: 1024, Index: 5,
	})

	out := buf.String()
	for _, want := range []string{"coordinator_selected", "chosen_ip=10.0.0.1", "is_self=true", "index=5"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in coord debug output; got %q", want, out)
		}
	}
}

// TestRefresh_CtxCanceledDoesNotBumpErrorCounter verifies that a
// refresh call whose ctx has been canceled (the normal shutdown
// path) does not bump consecutiveRefreshErrors or churn the stored
// peer-set into the self-only fallback. Without this guard the
// final refresh during graceful shutdown produces a 'discovery
// failed' warning and pushes the membership into the self-only
// path even though nothing has actually gone wrong.
func TestRefresh_CtxCanceledDoesNotBumpErrorCounter(t *testing.T) {
	t.Parallel()

	good := []Peer{
		{IP: "10.0.0.1", Self: false},
		{IP: "10.0.0.2", Self: true},
	}

	var failWithCancel atomic.Bool

	src := &fakePeerSource{
		mu: func() ([]Peer, error) {
			if failWithCancel.Load() {
				return nil, context.Canceled
			}

			out := make([]Peer, len(good))
			copy(out, good)

			return out, nil
		},
	}

	c, err := New(t.Context(),
		config.Cluster{
			Service:           "test",
			SelfPodIP:         "10.0.0.2",
			MembershipRefresh: time.Hour, // disable auto-refresh; drive manually.
		},
		WithPeerSource(src),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = c.Close(context.Background()) })

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Fatalf("pre-test error counter = %d, want 0", got)
	}

	initialPeers := len(c.Peers())

	failWithCancel.Store(true)
	c.refresh(t.Context())

	if got := c.consecutiveRefreshErrors.Load(); got != 0 {
		t.Errorf("counter bumped on ctx.Canceled; got %d want 0", got)
	}

	if got := len(c.Peers()); got != initialPeers {
		t.Errorf("peer-set churned on ctx.Canceled; got %d want %d", got, initialPeers)
	}
}

// TestDecodeChunkKey_RejectsZeroObjectSize verifies that the wire
// boundary rejects object_size == 0 as well as negative values.
// The previous code accepted 0 as a sentinel for "unknown size"
// which became a foot-gun (validation skipped, malformed range,
// validating-reader bypassed); production callers always know the
// size from a prior Head, so tightening the contract removes the
// foot-gun without breaking any real caller.
//
// Regression for C-2 / C-3 / C-4.
func TestDecodeChunkKey_RejectsZeroObjectSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		objectSize string
		wantErr    bool
	}{
		{"zero rejected", "0", true},
		{"negative rejected", "-1", true},
		{"positive accepted", "1024", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := url.Values{}
			v.Set("origin_id", "ox")
			v.Set("bucket", "b")
			v.Set("key", "o")
			v.Set("etag", "e1")
			v.Set("chunk_size", "1024")
			v.Set("index", "0")
			v.Set("object_size", tt.objectSize)

			_, _, err := DecodeChunkKey(v)
			if tt.wantErr {
				if err == nil {
					t.Errorf("DecodeChunkKey(object_size=%s) returned nil; want error", tt.objectSize)
				} else if !strings.Contains(err.Error(), "object_size") {
					t.Errorf("error does not mention object_size: %v", err)
				}

				return
			}

			if err != nil {
				t.Errorf("DecodeChunkKey(object_size=%s) unexpected error: %v", tt.objectSize, err)
			}
		})
	}
}
