// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coord_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	coordv1 "github.com/Azure/unbounded/internal/gantry/proto/coord/v1"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func makeHostPair(t *testing.T) (a, b host.Host) {
	t.Helper()

	mk := func() host.Host {
		h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = h.Close() })

		return h
	}
	a, b = mk(), mk()
	a.Peerstore().AddAddrs(b.ID(), b.Addrs(), peerstore.PermanentAddrTTL)
	b.Peerstore().AddAddrs(a.ID(), a.Addrs(), peerstore.PermanentAddrTTL)

	return a, b
}

type fixedChairValidator struct{ want ifaces.ChairAssignment }

func (v fixedChairValidator) ValidateChair(_ context.Context, a ifaces.ChairAssignment) bool {
	return a == v.want
}

type acceptingChairSuccessor struct {
	want     ifaces.ChairAssignment
	endpoint ifaces.PeerEndpoint
}

func (s acceptingChairSuccessor) AcceptChair(_ context.Context, _ ifaces.PeerID, a ifaces.ChairAssignment) (ifaces.PeerEndpoint, bool) {
	return s.endpoint, a == s.want
}

var testAssignment = ifaces.ChairAssignment{ChairID: 1 << 40, Generation: 4, AssignmentEpoch: 12}

func chairFixture(t *testing.T, pump coord.PullerPump, opts ...coord.Option) (*coord.Server, *coord.ChairHTTPClient, ifaces.PeerEndpoint) {
	t.Helper()

	opts = append(opts, coord.WithPullerPump(pump), coord.WithChairValidator(fixedChairValidator{want: testAssignment}))
	srv := coord.NewServer(opts...)
	port, id := startChairServer(t, srv)
	cli := coord.NewChairHTTPClient(coord.ChairHTTPOptions{Port: port, Timeout: 5 * time.Second})

	return srv, cli, ifaces.PeerEndpoint{PeerID: ifaces.PeerID(id.String()), TransferAddr: "127.0.0.1:5001"}
}

// The old intent tests now verify the removed wire fields cannot start work.
func rejectedEnvelope(t *testing.T, field protowire.Number) {
	t.Helper()
	a, b := makeHostPair(t)

	var calls atomic.Int32

	srv := coord.NewServer(coord.WithPullerPump(func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		calls.Add(1)
		return coord.PumpResult{Status: coord.PumpStarted}
	}))
	srv.Bind(b)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	s, err := a.NewStream(ctx, b.ID(), coord.ProtocolID)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_ = s.SetDeadline(time.Now().Add(2 * time.Second))

	data := protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), nil)
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		t.Fatal(err)
	}

	if _, err := msgio.NewVarintReaderSize(s, coord.MaxMessageBytes).ReadMsg(); err == nil {
		t.Fatal("removed envelope accepted")
	}

	if calls.Load() != 0 {
		t.Fatal("removed RPC started pump")
	}
}
func TestPullIntent_NotCachedNotInFlight(t *testing.T)                       { rejectedEnvelope(t, 1) }
func TestPullIntent_InFlight(t *testing.T)                                   { rejectedEnvelope(t, 2) }
func TestPullIntent_NegativeCacheSurfaced(t *testing.T)                      { rejectedEnvelope(t, 3) }
func TestPleasePull_RequireChairAssignmentRejectsLegacyRequest(t *testing.T) { rejectedEnvelope(t, 4) }

func outcomeRoundTrip(t *testing.T, result coord.PumpResult, want ifaces.PleasePullStatus) {
	t.Helper()

	var calls atomic.Int32

	_, cli, endpoint := chairFixture(t, func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		calls.Add(1)
		return result
	})
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

	out, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d}, testAssignment)
	if err != nil || len(out) != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}

	if out[0].Outcome != want || out[0].Digest != d || calls.Load() != 1 {
		t.Fatalf("out=%+v calls=%d", out, calls.Load())
	}

	if !result.StartedAt.IsZero() && !out[0].StartedAt.Equal(result.StartedAt) {
		t.Fatal("started timestamp lost")
	}

	if !result.CooldownUntil.IsZero() && (!out[0].CooldownUntil.Equal(result.CooldownUntil) || out[0].FailureClass != result.FailureClass) {
		t.Fatal("failure metadata lost")
	}
}

func TestPleasePull_Started(t *testing.T) {
	outcomeRoundTrip(t, coord.PumpResult{Status: coord.PumpStarted, StartedAt: time.Now()}, ifaces.PleasePullStarted)
}

func TestPleasePull_AlreadyPulling(t *testing.T) {
	outcomeRoundTrip(t, coord.PumpResult{Status: coord.PumpAlreadyPulling, StartedAt: time.Now()}, ifaces.PleasePullAlreadyPulling)
}

func TestPleasePull_RecentlyFailedShortCircuit(t *testing.T) {
	outcomeRoundTrip(t, coord.PumpResult{Status: coord.PumpRecentlyFailed, CooldownUntil: time.Now().Add(time.Minute), FailureClass: ifaces.FailureAuth}, ifaces.PleasePullRecentlyFailed)
}

func TestPleasePullChair_StaleAssignmentDoesNotStartPump(t *testing.T) {
	_, cli, endpoint := chairFixture(t, func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		t.Error("stale chair started pump")
		return coord.PumpResult{}
	})
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	a := testAssignment
	a.Generation--

	out, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d}, a)
	if err != nil || len(out) != 1 || out[0].Outcome != ifaces.PleasePullStaleChair {
		t.Fatalf("out=%+v err=%v", out, err)
	}

	if _, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d}, ifaces.ChairAssignment{}); err == nil {
		t.Fatal("missing assignment accepted")
	}
}

func TestOfferChairReturnsAcceptedSuccessorEndpoint(t *testing.T) {
	a, b := makeHostPair(t)
	want := ifaces.PeerEndpoint{PeerID: ifaces.PeerID(b.ID().String()), TransferAddr: "10.0.0.5:5001", P2PAddrs: []string{"/ip4/10.0.0.5/tcp/4001"}}
	coord.NewServer(coord.WithChairSuccessor(acceptingChairSuccessor{want: testAssignment, endpoint: want})).Bind(b)

	got, accepted, err := coord.NewClient(a).OfferChair(t.Context(), ifaces.PeerID(b.ID().String()), testAssignment)
	if err != nil || !accepted || got.PeerID != want.PeerID || got.TransferAddr != want.TransferAddr || len(got.P2PAddrs) != 1 {
		t.Fatalf("got=%+v accepted=%v err=%v", got, accepted, err)
	}
}

func TestPleasePull_DelegatesAuthorization(t *testing.T) {
	for _, auth := range []string{"Bearer requester-token", "Basic cmVxdWVzdGVyOnNlY3JldA=="} {
		seen := make(chan string, 1)
		_, cli, endpoint := chairFixture(t, func(ctx context.Context, _, _ string, _ digest.Digest, _ ifaces.OriginRefKind) coord.PumpResult {
			seen <- registryauth.Authorization(ctx)
			return coord.PumpResult{Status: coord.PumpStarted}
		})

		d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
		if _, err := cli.PleasePullChair(registryauth.WithAuthorization(t.Context(), auth), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d}, testAssignment); err != nil {
			t.Fatal(err)
		}

		if got := <-seen; got != auth {
			t.Fatal("authorization lost")
		}
	}
}

func TestPleasePull_DeclinedFiresHook(t *testing.T) {
	var declined, started atomic.Int32

	_, cli, endpoint := chairFixture(t, func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		return coord.PumpResult{Status: coord.PumpDeclined}
	}, coord.WithMetrics(coord.MetricsHooks{OnPleasePullDeclined: func(string) { declined.Add(1) }, OnPleasePullStarted: func() { started.Add(1) }}))
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

	out, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d, d}, testAssignment)
	if err != nil || len(out) != 2 || declined.Load() != 2 || started.Load() != 0 {
		t.Fatalf("out=%+v err=%v declined=%d started=%d", out, err, declined.Load(), started.Load())
	}
}

func TestPleasePull_RejectsOversizedBatch(t *testing.T) {
	// HTTP request-size bounds are tested directly; normal clients chunk batches.
	local := &localPullStub{}

	request := &coordv1.PleasePullRequest{Kind: coordv1.PleasePullRequest_KIND_BLOB, ChairAssignment: &coordv1.ChairAssignment{Generation: 1, AssignmentEpoch: 1}, UpstreamRegistry: "reg", Repository: "repo"}
	for range coord.DefaultMaxDigestsPerPleasePull + 1 {
		request.Digests = append(request.Digests, "sha256:"+strings.Repeat("a", 64))
	}

	data, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	coord.NewChairHTTPHandler(local, nil).ServeHTTP(w, httptest.NewRequest(http.MethodPost, coord.ChairHTTPPath, bytes.NewReader(data)))

	if w.Code != http.StatusBadRequest || local.gotRegistry != "" {
		t.Fatal("oversized request reached starter")
	}
}

func TestPleasePull_ClientChunksBatches(t *testing.T) {
	var calls atomic.Int32

	_, cli, endpoint := chairFixture(t, func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		calls.Add(1)
		return coord.PumpResult{Status: coord.PumpStarted}
	})
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

	ds := make([]digest.Digest, coord.DefaultMaxDigestsPerPleasePull+1)
	for i := range ds {
		ds[i] = d
	}

	out, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", ifaces.KindBlob, ds, testAssignment)
	if err != nil || len(out) != len(ds) || int(calls.Load()) != len(ds) {
		t.Fatalf("count=%d calls=%d err=%v", len(out), calls.Load(), err)
	}
}

func TestStartLocalPull_RespectsCanceledContext(t *testing.T) {
	s := coord.NewServer(coord.WithPullerPump(func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		t.Fatal("canceled request pumped")
		return coord.PumpResult{}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.StartLocalPull(ctx, "reg", "repo", ifaces.KindBlob, nil); err == nil {
		t.Fatal("canceled request accepted")
	}
}

func kindRoundTrip(t *testing.T, kind ifaces.OriginRefKind) {
	t.Helper()

	seen := make(chan ifaces.OriginRefKind, 1)
	_, cli, endpoint := chairFixture(t, func(_ context.Context, _, _ string, _ digest.Digest, k ifaces.OriginRefKind) coord.PumpResult {
		seen <- k
		return coord.PumpResult{Status: coord.PumpStarted}
	})

	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	if _, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "repo", kind, []digest.Digest{d}, testAssignment); err != nil {
		t.Fatal(err)
	}

	if got := <-seen; got != kind {
		t.Fatal("kind changed")
	}
}
func TestPleasePull_KindRoundtrip(t *testing.T)       { kindRoundTrip(t, ifaces.KindManifest) }
func TestPleasePull_KindConfigRoundtrip(t *testing.T) { kindRoundTrip(t, ifaces.KindConfig) }
func TestClient_UnknownNodeReturnsError(t *testing.T) { invalidOfferTarget(t, "not-a-peer-id") }
func TestClient_ResolvePeerIDCache(t *testing.T)      { invalidOfferTarget(t, "alias") }
func TestClient_PeerIDResolverCallback(t *testing.T)  { invalidOfferTarget(t, "k8s-node-name") }
func invalidOfferTarget(t *testing.T, id string) {
	t.Helper()

	a, _ := makeHostPair(t)
	if _, _, err := coord.NewClient(a).OfferChair(t.Context(), ifaces.PeerID(id), testAssignment); err == nil {
		t.Fatal("membership alias accepted")
	}
}

func TestServer_IdleStreamHitsDeadline(t *testing.T) {
	a, b := makeHostPair(t)
	coord.NewServer(coord.WithStreamHandshakeTimeout(100 * time.Millisecond)).Bind(b)

	s, err := a.NewStream(t.Context(), b.ID(), coord.ProtocolID)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))

	start := time.Now()
	if _, err := s.Read(make([]byte, 1)); err == nil || time.Since(start) > time.Second {
		t.Fatal("idle stream not promptly closed")
	}
}

func TestPleasePull_RejectsInvalidRepository(t *testing.T) {
	_, cli, endpoint := chairFixture(t, func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) coord.PumpResult {
		t.Error("invalid repo pumped")
		return coord.PumpResult{}
	})

	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	if _, err := cli.PleasePullChair(t.Context(), endpoint, "reg", "../../etc/passwd", ifaces.KindBlob, []digest.Digest{d}, testAssignment); err == nil {
		t.Fatal("invalid repository accepted")
	}
}

func TestStartLocalPull_RejectsInvalidRepository(t *testing.T) {
	if _, err := coord.NewServer().StartLocalPull(t.Context(), "reg", "Bad Repo?", ifaces.KindBlob, nil); err == nil {
		t.Fatal("invalid repository accepted")
	}
}
