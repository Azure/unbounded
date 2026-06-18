// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coord_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"

	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
)

// helper: build two libp2p hosts that know each other's addresses.
func makeHostPair(t *testing.T) (a, b host.Host) {
	t.Helper()

	mkHost := func() host.Host {
		h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatalf("libp2p.New: %v", err)
		}

		return h
	}
	a = mkHost()
	b = mkHost()
	a.Peerstore().AddAddrs(b.ID(), b.Addrs(), peerstore.PermanentAddrTTL)
	b.Peerstore().AddAddrs(a.ID(), a.Addrs(), peerstore.PermanentAddrTTL)
	t.Cleanup(func() {
		_ = a.Close() //nolint:errcheck // best-effort close
		_ = b.Close() //nolint:errcheck // best-effort close
	})

	return a, b
}

func TestPullIntent_NotCachedNotInFlight(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()

	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: ifaces.NodeID(hServer.ID().String()), Addr: "x"},
		ifaces.Node{ID: ifaces.NodeID(hClient.ID().String()), Addr: "y"},
	)
	infl := inflight.New(inflight.DefaultStalls(), nil)

	srv := coord.NewServer(c, members, infl)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	d := digest.MustParse("sha256:" + rep('a', 64))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	intent, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d)
	if err != nil {
		t.Fatalf("PullIntentQuery: %v", err)
	}

	if intent.HasCached {
		t.Error("unexpected HasCached=true")
	}

	if intent.InFlight {
		t.Error("unexpected InFlight=true")
	}

	if intent.RecipientRank < 0 {
		t.Errorf("RecipientRank = %d, want >=0", intent.RecipientRank)
	}
}

func TestPullIntent_InFlight(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: ifaces.NodeID(hServer.ID().String()), Addr: "x"},
	)
	infl := inflight.New(inflight.DefaultStalls(), nil)
	d := digest.MustParse("sha256:" + rep('b', 64))

	h, _, _ := infl.Start(d, ifaces.KindBlob, 0)
	defer h.Done()

	srv := coord.NewServer(c, members, infl)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	intent, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d)
	if err != nil {
		t.Fatalf("PullIntentQuery: %v", err)
	}

	if !intent.InFlight {
		t.Error("InFlight=false, want true")
	}

	if intent.StartedAt.IsZero() {
		t.Error("StartedAt zero, want set")
	}
}

func TestPleasePull_Started(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: ifaces.NodeID(hServer.ID().String()), Addr: "x"},
	)
	infl := inflight.New(inflight.DefaultStalls(), nil)

	var pumpCalls int32

	pump := coord.PullerPump(func(ctx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) (time.Time, bool, *coord.NegativeEntry) {
		atomic.AddInt32(&pumpCalls, 1)
		// Claim in-flight as the real puller would; that gates re-pulls.
		h, _, already := infl.Start(d, kind, 0)
		_ = h // leak intentionally for test brevity //nolint:errcheck // best-effort

		return time.Now(), already, nil
	})
	srv := coord.NewServer(c, members, infl, coord.WithPullerPump(pump))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d1 := digest.MustParse("sha256:" + rep('1', 64))
	d2 := digest.MustParse("sha256:" + rep('2', 64))

	outs, err := cli.PleasePull(ctx, ifaces.NodeID(hServer.ID().String()), "reg", "repo", ifaces.KindBlob, []digest.Digest{d1, d2})
	if err != nil {
		t.Fatalf("PleasePull: %v", err)
	}

	if len(outs) != 2 {
		t.Fatalf("len(outs) = %d, want 2", len(outs))
	}

	for _, o := range outs {
		if o.Outcome != ifaces.PleasePullStarted {
			t.Errorf("outcome for %s = %v, want PleasePullStarted", o.Digest, o.Outcome)
		}
	}

	if atomic.LoadInt32(&pumpCalls) != 2 {
		t.Errorf("pumpCalls = %d, want 2", pumpCalls)
	}
}

func TestPleasePull_AlreadyPulling(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)

	d := digest.MustParse("sha256:" + rep('c', 64))

	pre, _, _ := infl.Start(d, ifaces.KindBlob, 0)
	defer pre.Done()

	pump := coord.PullerPump(func(_ context.Context, _, _ string, d digest.Digest, kind ifaces.OriginRefKind) (time.Time, bool, *coord.NegativeEntry) {
		h, e, already := infl.Start(d, kind, 0)
		_ = h //nolint:errcheck // best-effort

		return e.StartedAt, already, nil
	})
	srv := coord.NewServer(c, members, infl, coord.WithPullerPump(pump))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	outs, err := cli.PleasePull(ctx, ifaces.NodeID(hServer.ID().String()), "reg", "repo", ifaces.KindBlob, []digest.Digest{d})
	if err != nil {
		t.Fatalf("PleasePull: %v", err)
	}

	if len(outs) != 1 || outs[0].Outcome != ifaces.PleasePullAlreadyPulling {
		t.Fatalf("outs = %+v; want ALREADY_PULLING", outs)
	}
}

// stubNegCache implements coord.NegativeCache for testing the design doc wiring.
type stubNegCache struct {
	entries map[digest.Digest]coord.NegativeEntry
}

func (s stubNegCache) Lookup(d digest.Digest) (coord.NegativeEntry, bool) {
	e, ok := s.entries[d]
	return e, ok
}

func TestPullIntent_NegativeCacheSurfaced(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: ifaces.NodeID(hServer.ID().String()), Addr: "x"},
	)
	infl := inflight.New(inflight.DefaultStalls(), nil)

	d := digest.MustParse("sha256:" + rep('f', 64))
	cooldownUntil := time.Now().Add(30 * time.Second).UTC().Truncate(time.Microsecond)
	neg := stubNegCache{entries: map[digest.Digest]coord.NegativeEntry{
		d: {CooldownUntil: cooldownUntil, Class: ifaces.FailureRateLimited},
	}}

	srv := coord.NewServer(c, members, infl, coord.WithNegativeCache(neg))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	intent, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d)
	if err != nil {
		t.Fatalf("PullIntentQuery: %v", err)
	}

	if !intent.RecentlyFailed {
		t.Fatalf("RecentlyFailed = false, want true")
	}

	if intent.FailureClass != ifaces.FailureRateLimited {
		t.Fatalf("FailureClass = %v, want FailureRateLimited", intent.FailureClass)
	}

	if !intent.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("CooldownUntil = %v, want %v", intent.CooldownUntil, cooldownUntil)
	}
}

func TestPleasePull_RecentlyFailedShortCircuit(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)

	d := digest.MustParse("sha256:" + rep('7', 64))
	cooldownUntil := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)

	// Pump returns *NegativeEntry to short-circuit (real puller would
	// consult its negcache before starting an origin pull).
	var pumpCalls int32

	pump := coord.PullerPump(func(_ context.Context, _, _ string, _ digest.Digest, _ ifaces.OriginRefKind) (time.Time, bool, *coord.NegativeEntry) {
		atomic.AddInt32(&pumpCalls, 1)

		return time.Time{}, false, &coord.NegativeEntry{
			CooldownUntil: cooldownUntil,
			Class:         ifaces.FailureAuth,
		}
	})
	srv := coord.NewServer(c, members, infl, coord.WithPullerPump(pump))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	outs, err := cli.PleasePull(ctx, ifaces.NodeID(hServer.ID().String()), "reg", "repo", ifaces.KindBlob, []digest.Digest{d})
	if err != nil {
		t.Fatalf("PleasePull: %v", err)
	}

	if len(outs) != 1 {
		t.Fatalf("len(outs) = %d, want 1", len(outs))
	}

	o := outs[0]
	if o.Outcome != ifaces.PleasePullRecentlyFailed {
		t.Fatalf("Outcome = %v, want PleasePullRecentlyFailed", o.Outcome)
	}

	if o.FailureClass != ifaces.FailureAuth {
		t.Fatalf("FailureClass = %v, want FailureAuth", o.FailureClass)
	}

	if !o.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("CooldownUntil = %v, want %v", o.CooldownUntil, cooldownUntil)
	}

	if got := atomic.LoadInt32(&pumpCalls); got != 1 {
		t.Fatalf("pumpCalls = %d, want 1", got)
	}
}

// TestPleasePull_KindRoundtrip asserts that the PleasePullRequest.kind
// field is encoded on the client and surfaced verbatim to the
// puller-pump on the server. Regression test for the earlier wire
// gap where the server hardcoded ifaces.KindBlob, causing manifest
// cold-start requests to be sent to /v2/<repo>/blobs/<digest>
// instead of /v2/<repo>/manifests/<digest>.
func TestPleasePull_KindRoundtrip(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)

	var (
		observedKind   ifaces.OriginRefKind
		observedDigest digest.Digest
	)

	pump := coord.PullerPump(func(_ context.Context, _, _ string, d digest.Digest, kind ifaces.OriginRefKind) (time.Time, bool, *coord.NegativeEntry) {
		observedKind = kind
		observedDigest = d
		h, _, already := infl.Start(d, kind, 0)
		_ = h //nolint:errcheck // best-effort

		return time.Now(), already, nil
	})
	srv := coord.NewServer(c, members, infl, coord.WithPullerPump(pump))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('b', 64))
	if _, err := cli.PleasePull(ctx, ifaces.NodeID(hServer.ID().String()), "reg", "repo", ifaces.KindManifest, []digest.Digest{d}); err != nil {
		t.Fatalf("PleasePull: %v", err)
	}

	if observedDigest != d {
		t.Fatalf("observedDigest = %s; want %s", observedDigest, d)
	}

	if observedKind != ifaces.KindManifest {
		t.Fatalf("observedKind = %v; want KindManifest", observedKind)
	}
}

// TestPleasePull_KindConfigRoundtrip extends the kind-roundtrip
// coverage to the new ifaces.KindConfig variant. The proto enum
// gained KIND_CONFIG so per-kind metrics ("manifest | config | layer")
// stay honest across the please_pull wire - without this round-trip
// a coordinator-driven origin pull of an image-config blob would
// downgrade to "blob" on the puller and the
// p2p_origin_pull_total{kind="config"} bucket would be permanently
// zero in production.
func TestPleasePull_KindConfigRoundtrip(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)

	var observedKind ifaces.OriginRefKind

	pump := coord.PullerPump(func(_ context.Context, _, _ string, d digest.Digest, kind ifaces.OriginRefKind) (time.Time, bool, *coord.NegativeEntry) {
		observedKind = kind
		h, _, already := infl.Start(d, kind, 0)
		_ = h //nolint:errcheck // best-effort

		return time.Now(), already, nil
	})
	srv := coord.NewServer(c, members, infl, coord.WithPullerPump(pump))
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('c', 64))
	if _, err := cli.PleasePull(ctx, ifaces.NodeID(hServer.ID().String()), "reg", "repo", ifaces.KindConfig, []digest.Digest{d}); err != nil {
		t.Fatalf("PleasePull: %v", err)
	}

	if observedKind != ifaces.KindConfig {
		t.Fatalf("observedKind = %v; want KindConfig (wire downgrade detected)", observedKind)
	}
}

func TestClient_UnknownNodeReturnsError(t *testing.T) {
	hClient, _ := makeHostPair(t)
	cli := coord.NewClient(hClient)

	// peer.Decode of a non-peerID string fails.
	d := digest.MustParse("sha256:" + rep('d', 64))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := cli.PullIntentQuery(ctx, ifaces.NodeID("not-a-peer-id"), d)
	if err == nil {
		t.Fatal("expected error for unresolvable NodeID")
	}
}

// Ensure peer.ID resolution caching works.
func TestClient_ResolvePeerIDCache(t *testing.T) {
	hClient, hServer := makeHostPair(t)
	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)
	srv := coord.NewServer(c, members, infl)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)
	cli.ResolvePeerID("alias", hServer.ID())

	d := digest.MustParse("sha256:" + rep('e', 64))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cli.PullIntentQuery(ctx, "alias", d); err != nil {
		t.Fatalf("aliased call: %v", err)
	}
}

// TestClient_PeerIDResolverCallback verifies that
// WithPeerIDResolver is consulted before ResolvePeerID's static cache
// and before peer.Decode fallback - so a NodeID that is neither in the
// cache nor a valid peer.ID string is still routable when the resolver
// returns a hit.
func TestClient_PeerIDResolverCallback(t *testing.T) {
	hClient, hServer := makeHostPair(t)
	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)
	srv := coord.NewServer(c, members, infl)
	srv.Bind(hServer)

	var calls int32

	cli := coord.NewClient(hClient,
		coord.WithPeerIDResolver(func(id ifaces.NodeID) (peer.ID, bool) {
			atomic.AddInt32(&calls, 1)

			if id == "k8s-node-name" {
				return hServer.ID(), true
			}

			return "", false
		}),
	)

	d := digest.MustParse("sha256:" + rep('f', 64))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cli.PullIntentQuery(ctx, "k8s-node-name", d); err != nil {
		t.Fatalf("resolver-routed call: %v", err)
	}

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("resolver fn was not consulted")
	}
}

// silence "imported and not used" for peer in some build matrices.
var _ peer.ID

// TestServer_IdleStreamHitsDeadline asserts the server-side handshake
// timeout: a peer that opens a coord stream and never writes the
// length-delimited envelope must not pin a goroutine indefinitely. We
// configure a short StreamHandshakeTimeout, open a raw stream, write
// nothing, and verify the server-side close lands well inside the
// test's own deadline.
func TestServer_IdleStreamHitsDeadline(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	c := fakes.NewCache()
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()))
	infl := inflight.New(inflight.DefaultStalls(), nil)

	srv := coord.NewServer(c, members, infl,
		coord.WithStreamHandshakeTimeout(150*time.Millisecond),
	)
	srv.Bind(hServer)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	str, err := hClient.NewStream(ctx, hServer.ID(), coord.ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	defer func() { _ = str.Close() }() //nolint:errcheck // best-effort close

	// Write nothing. The server must hit its read deadline and close
	// the stream; the client-side Read should observe EOF / reset.
	buf := make([]byte, 4)
	readDone := make(chan error, 1)

	go func() {
		_, rerr := str.Read(buf)
		readDone <- rerr
	}()

	select {
	case <-readDone:
		// Good \u2014 server closed the stream within the handshake window.
	case <-time.After(2 * time.Second):
		t.Fatal("server did not close idle stream within deadline; slowloris guard regressed")
	}
}

func rep(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}

	return string(b)
}

// TestPeerAuthz_ObserveOnlyServesAndCounts asserts that, with enforcement
// off (the default), an inbound request from a peer absent from the
// membership view is still served but increments the unauthorized-peer
// metric so operators can size the false-positive rate.
func TestPeerAuthz_ObserveOnlyServesAndCounts(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	// Membership knows the server but NOT the client peer, and publishes a
	// non-empty PeerID so authorization can actually be evaluated.
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: "server-node", PeerID: hServer.ID().String()},
	)

	var (
		unauthorized int32
		reason       atomic.Pointer[string]
	)

	srv := coord.NewServer(fakes.NewCache(), members, inflight.New(inflight.DefaultStalls(), nil),
		coord.WithMetrics(coord.MetricsHooks{
			OnUnauthorizedPeer: func(r string) { atomic.AddInt32(&unauthorized, 1); reason.Store(&r) },
		}),
	)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('a', 64))
	if _, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d); err != nil {
		t.Fatalf("PullIntentQuery (observe-only must still serve): %v", err)
	}

	if got := atomic.LoadInt32(&unauthorized); got != 1 {
		t.Fatalf("unauthorized metric = %d, want 1", got)
	}

	if r := reason.Load(); r == nil || *r != "unrecognized" {
		t.Fatalf("reason = %v, want \"unrecognized\"", r)
	}
}

// TestPeerAuthz_EnforceRejectsUnknownPeer asserts that, with enforcement
// on, an inbound request from a peer absent from the membership view is
// rejected before dispatch and still counted.
func TestPeerAuthz_EnforceRejectsUnknownPeer(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: "server-node", PeerID: hServer.ID().String()},
	)

	var (
		unauthorized int32
		reason       atomic.Pointer[string]
	)

	srv := coord.NewServer(fakes.NewCache(), members, inflight.New(inflight.DefaultStalls(), nil),
		coord.WithPeerAuthz(true),
		coord.WithMetrics(coord.MetricsHooks{
			OnUnauthorizedPeer: func(r string) { atomic.AddInt32(&unauthorized, 1); reason.Store(&r) },
		}),
	)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('a', 64))
	if _, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d); err == nil {
		t.Fatal("PullIntentQuery: want rejection under enforce mode, got nil error")
	}

	if got := atomic.LoadInt32(&unauthorized); got != 1 {
		t.Fatalf("unauthorized metric = %d, want 1", got)
	}

	if r := reason.Load(); r == nil || *r != "unrecognized" {
		t.Fatalf("reason = %v, want \"unrecognized\"", r)
	}
}

// TestPeerAuthz_EnforceAllowsKnownPeer asserts an authorized peer (its
// libp2p peer ID is published in the membership view) is served under
// enforce mode and never trips the unauthorized-peer metric.
func TestPeerAuthz_EnforceAllowsKnownPeer(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: "server-node", PeerID: hServer.ID().String()},
		ifaces.Node{ID: "client-node", PeerID: hClient.ID().String()},
	)

	var unauthorized int32

	srv := coord.NewServer(fakes.NewCache(), members, inflight.New(inflight.DefaultStalls(), nil),
		coord.WithPeerAuthz(true),
		coord.WithMetrics(coord.MetricsHooks{
			OnUnauthorizedPeer: func(string) { atomic.AddInt32(&unauthorized, 1) },
		}),
	)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('a', 64))
	if _, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d); err != nil {
		t.Fatalf("PullIntentQuery (known peer must be served): %v", err)
	}

	if got := atomic.LoadInt32(&unauthorized); got != 0 {
		t.Fatalf("unauthorized metric = %d, want 0", got)
	}
}

// TestPeerAuthz_ObserveOnlyFailsOpenWhenNoPeerIDsPublished asserts that when
// no member has published a libp2p peer ID yet (cold boot, informer lag),
// observe-only mode does not generate unauthorized-peer noise and still serves.
func TestPeerAuthz_ObserveOnlyFailsOpenWhenNoPeerIDsPublished(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	// Members exist but none have a published PeerID (annotation not set
	// yet). In observe-only mode, authorization is unevaluable -> fail open
	// without metric noise.
	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: "server-node", Addr: "x"},
		ifaces.Node{ID: "client-node", Addr: "y"},
	)

	var unauthorized int32

	srv := coord.NewServer(fakes.NewCache(), members, inflight.New(inflight.DefaultStalls(), nil),
		coord.WithMetrics(coord.MetricsHooks{
			OnUnauthorizedPeer: func(string) { atomic.AddInt32(&unauthorized, 1) },
		}),
	)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('a', 64))
	if _, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d); err != nil {
		t.Fatalf("PullIntentQuery (observe-only unevaluable authz must serve): %v", err)
	}

	if got := atomic.LoadInt32(&unauthorized); got != 0 {
		t.Fatalf("unauthorized metric = %d, want 0 (cannot evaluate -> no miss)", got)
	}
}

// TestPeerAuthz_EnforceRejectsWhenNoPeerIDsPublished asserts that enforcement
// is a hard gate. If no PeerIDs are published, authorization is unevaluable and
// must reject rather than silently failing open.
func TestPeerAuthz_EnforceRejectsWhenNoPeerIDsPublished(t *testing.T) {
	hClient, hServer := makeHostPair(t)

	members := fakes.NewMembers(ifaces.NodeID(hServer.ID().String()),
		ifaces.Node{ID: "server-node", Addr: "x"},
		ifaces.Node{ID: "client-node", Addr: "y"},
	)

	var (
		unauthorized int32
		reason       atomic.Pointer[string]
	)

	srv := coord.NewServer(fakes.NewCache(), members, inflight.New(inflight.DefaultStalls(), nil),
		coord.WithPeerAuthz(true),
		coord.WithMetrics(coord.MetricsHooks{
			OnUnauthorizedPeer: func(r string) { atomic.AddInt32(&unauthorized, 1); reason.Store(&r) },
		}),
	)
	srv.Bind(hServer)

	cli := coord.NewClient(hClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := digest.MustParse("sha256:" + rep('a', 64))
	if _, err := cli.PullIntentQuery(ctx, ifaces.NodeID(hServer.ID().String()), d); err == nil {
		t.Fatal("PullIntentQuery: want rejection when authz is unevaluable under enforce mode, got nil error")
	}

	if got := atomic.LoadInt32(&unauthorized); got != 1 {
		t.Fatalf("unauthorized metric = %d, want 1", got)
	}

	if r := reason.Load(); r == nil || *r != "unevaluable" {
		t.Fatalf("reason = %v, want \"unevaluable\"", r)
	}
}
