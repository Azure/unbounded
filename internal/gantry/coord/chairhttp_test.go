// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coord_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Azure/unbounded/internal/gantry/chaircall"
	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type localPullStub struct {
	gotRegistry      string
	gotRepository    string
	gotAssignment    ifaces.ChairAssignment
	gotAuthorization string
	outcome          ifaces.PleasePullStatus
}

func (s *localPullStub) StartLocalChairPull(ctx context.Context, registry, repository string, _ ifaces.OriginRefKind, digests []digest.Digest, assignment ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	s.gotRegistry = registry
	s.gotRepository = repository
	s.gotAssignment = assignment
	s.gotAuthorization = registryauth.Authorization(ctx)

	out := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, d := range digests {
		out = append(out, ifaces.PleasePullOutcome{Digest: d, Outcome: s.outcome, StartedAt: time.Now()})
	}

	return out, nil
}

// startChairServer runs the HTTPS please_pull listener on a loopback port and
// returns the port plus the server's peer ID.
func startChairServer(t *testing.T, local ifaces.LocalChairPullStarter) (int, peer.ID) {
	t.Helper()

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("IDFromPrivateKey: %v", err)
	}

	cfg, err := chaircall.ServerTLSConfig(priv)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: coord.NewChairHTTPHandler(local, nil), ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = srv.Serve(ln) }() //nolint:errcheck // closed via cleanup

	t.Cleanup(func() { _ = srv.Close() }) //nolint:errcheck // test cleanup

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	return port, id
}

func TestChairHTTPRoundTripDeliversRequestAndOutcome(t *testing.T) {
	local := &localPullStub{outcome: ifaces.PleasePullStarted}
	port, id := startChairServer(t, local)

	cli := coord.NewChairHTTPClient(coord.ChairHTTPOptions{Port: port, Timeout: 10 * time.Second})

	ctx := registryauth.WithAuthorization(context.Background(), "Bearer requester-token")
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	assignment := ifaces.ChairAssignment{ChairID: 7, Generation: 3, AssignmentEpoch: 11}

	endpoint := ifaces.PeerEndpoint{
		PeerID:       ifaces.NodeID(id.String()),
		TransferAddr: net.JoinHostPort("127.0.0.1", "5001"),
	}

	outs, err := cli.PleasePullChair(ctx, endpoint, "reg.example.com", "lib/app", ifaces.KindBlob, []digest.Digest{d}, assignment)
	if err != nil {
		t.Fatalf("PleasePullChair: %v", err)
	}

	if len(outs) != 1 || outs[0].Outcome != ifaces.PleasePullStarted {
		t.Fatalf("outcomes = %+v, want one Started", outs)
	}

	if local.gotRegistry != "reg.example.com" || local.gotRepository != "lib/app" {
		t.Fatalf("server saw %s/%s", local.gotRegistry, local.gotRepository)
	}

	if local.gotAssignment != assignment {
		t.Fatalf("assignment = %+v, want %+v", local.gotAssignment, assignment)
	}

	// The delegated credential is the reason this transport needs TLS; losing it
	// would silently break private-registry pulls.
	if local.gotAuthorization != "Bearer requester-token" {
		t.Fatalf("authorization = %q, want the delegated credential", local.gotAuthorization)
	}
}

// The peer ID pin is the entire trust decision, so a chair answering on the
// expected address with a different identity must be refused.
func TestChairHTTPRejectsUnexpectedPeerIdentity(t *testing.T) {
	local := &localPullStub{outcome: ifaces.PleasePullStarted}
	port, _ := startChairServer(t, local)

	_, otherPub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	impostor, err := peer.IDFromPublicKey(otherPub)
	if err != nil {
		t.Fatalf("IDFromPublicKey: %v", err)
	}

	cli := coord.NewChairHTTPClient(coord.ChairHTTPOptions{Port: port, Timeout: 10 * time.Second})

	endpoint := ifaces.PeerEndpoint{
		PeerID:       ifaces.NodeID(impostor.String()),
		TransferAddr: net.JoinHostPort("127.0.0.1", "5001"),
	}

	d := digest.MustParse("sha256:" + strings.Repeat("b", 64))

	_, err = cli.PleasePullChair(context.Background(), endpoint, "reg", "repo", ifaces.KindBlob, []digest.Digest{d}, ifaces.ChairAssignment{})
	if err == nil {
		t.Fatal("call succeeded against a chair with an unexpected identity")
	}
}

func TestChairHTTPRequiresTransferAddress(t *testing.T) {
	cli := coord.NewChairHTTPClient(coord.ChairHTTPOptions{Port: 5002})

	d := digest.MustParse("sha256:" + strings.Repeat("c", 64))

	_, err := cli.PleasePullChair(context.Background(),
		ifaces.PeerEndpoint{PeerID: "12D3KooWQV36jLPPuvLfLkG1eaFixmdwg16AsXdQQm97EGovXoZ3"},
		"reg", "repo", ifaces.KindBlob, []digest.Digest{d}, ifaces.ChairAssignment{})
	if err == nil {
		t.Fatal("expected an error when the endpoint has no transfer address")
	}
}

func TestChairHTTPHandlerRejectsNonPost(t *testing.T) {
	local := &localPullStub{outcome: ifaces.PleasePullStarted}
	h := coord.NewChairHTTPHandler(local, nil)

	rec := &statusRecorder{header: http.Header{}}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, coord.ChairHTTPPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	h.ServeHTTP(rec, req)

	if rec.status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.status)
	}
}

type statusRecorder struct {
	header http.Header
	status int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.status = code }

// TestChairHTTPDialIsBoundedAgainstStalledTLS covers a chair address that
// accepts TCP and then never negotiates TLS. net/http establishes connections
// on a context detached from the requesting call, so without a deadline inside
// the dial each attempt would leak a socket and a goroutine.
func TestChairHTTPDialIsBoundedAgainstStalledTLS(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	accepted := make(chan net.Conn, 8)

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			// Never speak TLS; just hold the connection open.
			accepted <- conn
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	_, pub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	pid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IDFromPublicKey: %v", err)
	}

	client := coord.NewChairHTTPClient(coord.ChairHTTPOptions{Port: port, Timeout: 2 * time.Second})
	endpoint := ifaces.PeerEndpoint{PeerID: ifaces.NodeID(pid.String()), TransferAddr: "127.0.0.1:1"}
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))

	start := time.Now()

	_, err = client.PleasePullChair(context.Background(), endpoint, "reg", "repo",
		ifaces.KindBlob, []digest.Digest{d}, ifaces.ChairAssignment{})
	if err == nil {
		t.Fatal("PleasePullChair against a stalled TLS listener returned nil error")
	}

	// The dial deadline must fire well inside the test budget rather than
	// hanging until the process exits.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("stalled dial took %v; want it bounded by the dial timeout", elapsed)
	}
}
