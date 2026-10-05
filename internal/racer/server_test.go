// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestHTTPSBootstrapSnapshotAndStrictRoutes(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	anonymous := f.client(t, nil)

	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := anonymous.Do(req)
	encoded := responseBody(t, response, err, 200)

	enrollment, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(enrollment.CertificateChain[0])
	if err != nil {
		t.Fatal(err)
	}

	if enrollment.Node != wire.NodeID(testNodeUID) || len(leaf.DNSNames) != 0 || leaf.Subject.CommonName != "" {
		t.Fatal("CSR became authority")
	}

	response, err = anonymous.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, 401)
	peer := f.client(t, &f.certificate)
	response, err = peer.Get(endpoint + wire.SnapshotPath)

	publication, err := wire.DecodePublication(bytes.NewReader(responseBody(t, response, err, 200)))
	if err != nil || len(publication.Members) != 1 {
		t.Fatalf("snapshot: %v", err)
	}

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/bootstrap", 400},
		{"POST", "/v1/snapshot", 400},
		{"GET", "/v1/snapshot/", 400},
		{"GET", "/v1//snapshot", 400},
		{"GET", "/v1/%73napshot", 400},
		{"GET", "/healthz", 400},
		{"GET", "/v1/snapshot?after=0", 409},
		{"GET", "/v1/snapshot?after=999", 503},
		{"GET", "/v1/snapshot?after=01", 400},
		{"GET", "/v1/snapshot?after=+1", 400},
		{"GET", "/v1/snapshot?after=%31", 400},
		{"GET", "/v1/snapshot?after=1&after=1", 400},
		{"GET", "/v1/snapshot?other=1", 400},
		{"GET", "/v1/snapshot?", 400},
	} {
		t.Run(tc.path+tc.method, func(t *testing.T) {
			r, err := http.NewRequestWithContext(f.ctx, tc.method, endpoint+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := peer.Do(r)
			responseBody(t, response, err, tc.status)
		})
	}
}

func TestAuthenticatedSharesProposalAndExplicitNodePrecedence(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	f.request.Shares = 9

	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.client(t, nil).Do(req)
	responseBody(t, response, err, 200)

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	if node.Annotations[enrolledSharesAnnotation] != "9" {
		t.Fatal("authenticated proposal not persisted")
	}

	published := reconcileTopology(t, f.a.Topology, f.ctx)

	publication, err := wire.DecodePublication(strings.NewReader(published.encoded))
	if err != nil || publication.Members[0].Shares != 9 {
		t.Fatalf("proposal not published: %+v %v", publication, err)
	}

	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Annotations[wire.SharesAnnotation] = "12"
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	published = reconcileTopology(t, f.a.Topology, f.ctx)

	publication, err = wire.DecodePublication(strings.NewReader(published.encoded))
	if err != nil || publication.Members[0].Shares != 12 {
		t.Fatalf("explicit shares lost: %+v %v", publication, err)
	}
}

func TestHTTPSBootstrapBoundsAndErrors(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	c := f.client(t, nil)

	valid, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, body, token, media string
		status                   int
	}{
		{"no token", string(valid), "", "application/json", 401},
		{"duplicate fields", `{"schema_version":1,"schema_version":1}`, f.token, "application/json", 400},
		{"unsupported", strings.Replace(string(valid), `"schema_version":1`, `"schema_version":2`, 1), f.token, "application/json", 426},
		{"large", strings.Repeat(" ", wire.MaxBootstrapBytes+1), f.token, "application/json", 413},
		{"media", string(valid), f.token, "text/plain", 400},
		{"cluster", strings.Replace(string(valid), string(f.request.Cluster), testNodeUID, 1), f.token, "application/json", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}

			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}

			r.Header.Set("Content-Type", tc.media)
			response, err := c.Do(r)
			responseBody(t, response, err, tc.status)
		})
	}
}

func TestHTTPRoutesBeforeReadinessAndTLS(t *testing.T) {
	f := newServingFixture(t)
	handler := f.a.Server.Handler()

	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", ready), func(t *testing.T) {
			f.a.Lifecycle.SetServingReady(ready)

			for _, tc := range []struct {
				method, path string
				valid        bool
			}{
				{http.MethodPost, wire.BootstrapPath, true},
				{http.MethodGet, wire.SnapshotPath, true},
				{http.MethodGet, wire.BootstrapPath, false},
				{http.MethodPost, wire.SnapshotPath, false},
				{http.MethodHead, wire.BootstrapPath, false},
				{http.MethodHead, wire.SnapshotPath, false},
				{http.MethodOptions, wire.BootstrapPath, false},
				{http.MethodOptions, wire.SnapshotPath, false},
				{http.MethodPost, "/unknown", false},
				{http.MethodGet, "/unknown", false},
				{http.MethodPost, "/v1/%62ootstrap", false},
				{http.MethodGet, "/v1/%73napshot", false},
			} {
				t.Run(tc.method+tc.path, func(t *testing.T) {
					r := httptest.NewRequest(tc.method, tc.path, nil)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)

					wantStatus, wantCode := http.StatusBadRequest, wire.InvalidRequest
					if tc.valid {
						wantStatus, wantCode = http.StatusServiceUnavailable, wire.Unavailable
						if ready {
							wantStatus, wantCode = http.StatusUnauthorized, wire.Unauthenticated
						}
					}

					body := responseBody(t, w.Result(), nil, wantStatus)
					if string(body) != fmt.Sprintf(`{"code":%q}`, wantCode) {
						t.Fatalf("response %s, want code %s", body, wantCode)
					}
				})
			}
		})
	}
}

func TestHTTPFailureResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		status     int
		code       wire.ErrorCode
		retryAfter string
	}{
		{"invalid request", wire.InvalidRequest, http.StatusBadRequest, wire.InvalidRequest, ""},
		{"unauthenticated", wire.Unauthenticated, http.StatusUnauthorized, wire.Unauthenticated, ""},
		{"forbidden", wire.Forbidden, http.StatusForbidden, wire.Forbidden, ""},
		{"conflict", wire.Conflict, http.StatusConflict, wire.Conflict, ""},
		{"too large", wire.TooLarge, http.StatusRequestEntityTooLarge, wire.TooLarge, ""},
		{"unsupported version", wire.UnsupportedVersion, http.StatusUpgradeRequired, wire.UnsupportedVersion, ""},
		{"overloaded", wire.Overloaded, http.StatusTooManyRequests, wire.Overloaded, "1"},
		{"unavailable", wire.Unavailable, http.StatusServiceUnavailable, wire.Unavailable, "1"},
		{"wrapped protocol error", fmt.Errorf("internal detail: %w", wire.Forbidden), http.StatusForbidden, wire.Forbidden, ""},
		{"unknown protocol error", wire.ErrorCode("unknown"), http.StatusServiceUnavailable, wire.Unavailable, "1"},
		{"nonprotocol error", io.ErrUnexpectedEOF, http.StatusServiceUnavailable, wire.Unavailable, "1"},
		{"nil error", nil, http.StatusServiceUnavailable, wire.Unavailable, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeFailure(w, tc.err)

			body := responseBody(t, w.Result(), nil, tc.status)
			if string(body) != fmt.Sprintf(`{"code":%q}`, tc.code) {
				t.Fatalf("response %s, want code %s", body, tc.code)
			}

			if got := w.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
			}

			if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" || !w.Flushed {
				t.Fatal("failure response headers or flush missing")
			}
		})
	}
}

func (f *servingFixture) signLeaf(t *testing.T, mutate func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	_, _, rotation, material := keyState(t, f.a.Keyring)

	ca, key, err := parseSigning(material.Keys[rotation.ActiveIssuer])
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(123), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "spiffe", Host: string(f.request.Cluster), Path: "/node/" + testNodeUID}}}
	mutate(template)

	der, err := x509.CreateCertificate(rand.Reader, template, ca, f.key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}

	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: f.key}
}

func TestHTTPSCertificateRejectionAndRecovery(t *testing.T) {
	f := newServingFixture(t)

	endpoint := f.start(t)
	for name, tc := range map[string]struct {
		mutate            func(*x509.Certificate)
		status            int
		allowTLSRejection bool
	}{
		"expired":       {func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Second) }, http.StatusUnauthorized, true},
		"future":        {func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) }, http.StatusUnauthorized, true},
		"wrong usage":   {func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }, http.StatusUnauthorized, true},
		"wrong cluster": {func(c *x509.Certificate) { c.URIs[0].Host = testNodeUID }, http.StatusForbidden, false},
		// A signed, well-formed UID need not appear in routing membership.
		"wrong uid":        {func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID }, http.StatusOK, false},
		"bad identity":     {func(c *x509.Certificate) { c.URIs[0].Path = "/node/not-a-uuid" }, http.StatusUnauthorized, false},
		"ambiguous SAN":    {func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) }, http.StatusUnauthorized, false},
		"query SAN":        {func(c *x509.Certificate) { c.URIs[0].RawQuery = "admin=true" }, http.StatusUnauthorized, false},
		"no signing usage": {func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment }, http.StatusUnauthorized, false},
	} {
		t.Run(name, func(t *testing.T) {
			cert := f.signLeaf(t, tc.mutate)

			response, err := f.client(t, &cert).Get(endpoint + wire.SnapshotPath)
			if err != nil && tc.allowTLSRejection && strings.Contains(err.Error(), "remote error: tls:") {
				return
			}

			if tc.status == http.StatusOK {
				responseBody(t, response, err, tc.status)
				return
			}
			// Require only the expected wire error, with no snapshot bytes admitted.
			want := map[int]string{
				http.StatusUnauthorized:       `{"code":"unauthenticated"}`,
				http.StatusForbidden:          `{"code":"forbidden"}`,
				http.StatusServiceUnavailable: `{"code":"unavailable"}`,
			}[tc.status]
			if body := responseBody(t, response, err, tc.status); string(body) != want {
				t.Fatalf("rejection response: %s, want %s", body, want)
			}
		})
	}
	// Expired identities can recover by omitting the certificate entirely.
	encoded, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err := f.client(t, nil).Do(req)

	enrollment, err := wire.DecodeBootstrapResponse(bytes.NewReader(responseBody(t, response, err, http.StatusOK)))
	if err != nil {
		t.Fatal(err)
	}

	if enrollment.Node != wire.NodeID(testNodeUID) {
		t.Fatalf("recovered Node %s, want %s", enrollment.Node, testNodeUID)
	}

	cert := tls.Certificate{Certificate: enrollment.CertificateChain, PrivateKey: f.key}
	response, err = f.client(t, &cert).Get(endpoint + wire.SnapshotPath)

	publication, err := wire.DecodePublication(bytes.NewReader(responseBody(t, response, err, http.StatusOK)))
	if err != nil || len(publication.Members) != 1 {
		t.Fatalf("recovered snapshot: %v", err)
	}
}

func TestPooledTLSIgnoresWorkloadChangesButRejectsExpiry(t *testing.T) {
	for _, scenario := range []string{"excluded", "recreated node", "pod gone", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)

			cert := f.certificate
			if scenario == "expired" {
				cert = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(2 * time.Second).Truncate(time.Second) })
			}

			endpoint := f.start(t)
			c := f.client(t, &cert)
			response, err := c.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, 200)

			switch scenario {
			case "excluded", "recreated node":
				node := &corev1.Node{}
				if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
					t.Fatal(err)
				}

				if scenario == "excluded" {
					node.Labels = map[string]string{wire.ExclusionLabel: ""}
				} else {
					node.UID = "replacement"
				}

				if err := f.a.Topology.Update(f.ctx, node); err != nil {
					t.Fatal(err)
				}
			case "pod gone":
				pod := &corev1.Pod{}
				if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
					t.Fatal(err)
				}

				if err := f.a.Topology.Delete(f.ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "expired":
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}

				time.Sleep(time.Until(leaf.NotAfter) + 10*time.Millisecond)
			}

			reused := false
			ctx := httptrace.WithClientTrace(f.ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

			req, err := http.NewRequestWithContext(ctx, "GET", endpoint+wire.SnapshotPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err = c.Do(req)

			want := 200

			switch scenario {
			case "expired":
				want = 401
			}

			responseBody(t, response, err, want)

			if !reused {
				t.Fatal("test failed to reuse TLS connection")
			}
		})
	}
}

func TestPooledTLSRetiredTrustAndNoResumption(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	c := f.client(t, &f.certificate)
	response, err := c.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, 200)
	// Install a coherent post-retirement credential state while the original
	// leaf is still valid. This isolates trust revocation from leaf expiration.
	shared, bundle, state, material := keyState(t, f.a.Keyring)

	der, key, err := generateIssuer(time.Now().UTC().Truncate(time.Second), f.a.Keyring.Config)
	if err != nil {
		t.Fatal(err)
	}

	id := rootID(der)
	material.Keys = map[string]signingMaterial{id: {Certificate: der, PrivateKey: key}}

	shared.Data["issuer.json"], err = json.Marshal(material)
	if err != nil {
		t.Fatal(err)
	}

	bundle.PeerTrustRoots = [][]byte{der}
	bundle.Generation++
	state.ActiveIssuer = id

	shared.Data["bundle.json"], err = wire.EncodeBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	shared.Data["rotation.json"], err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.a.Topology.Update(f.ctx, shared); err != nil {
		t.Fatal(err)
	}

	runKeys(t, f.a.Keyring)

	reused := false
	ctx := httptrace.WithClientTrace(f.ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+wire.SnapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err = c.Do(req)
	responseBody(t, response, err, 401)

	if !reused {
		t.Fatal("trust rotation did not use pooled connection")
	}

	c.Transport.(*http.Transport).CloseIdleConnections()

	response, err = c.Get(endpoint + wire.SnapshotPath)
	if err == nil {
		response.Body.Close()
		t.Fatal("new TLS connection accepted retired root")
	}
	// A fresh identity reconnects successfully but cannot resume an old session.
	encoded, err := f.a.authority.Issue(f.ctx, fixtureIdentity(t, f), f.request)
	if err != nil {
		t.Fatal(err)
	}

	responseChain := decodeIssuedResponse(t, encoded)

	fresh := tls.Certificate{Certificate: responseChain.CertificateChain, PrivateKey: f.key}

	freshClient := f.client(t, &fresh)
	for range 2 {
		response, err = freshClient.Get(endpoint + wire.SnapshotPath)
		responseBody(t, response, err, 200)

		if response.TLS.DidResume {
			t.Fatal("TLS resumed authentication")
		}

		freshClient.Transport.(*http.Transport).CloseIdleConnections()
	}
}

type blockingResponse struct {
	*httptest.ResponseRecorder
	entered, unblock chan struct{}
	once             sync.Once
	blockFlush       bool
	fail             bool
}

func (w *blockingResponse) Write(b []byte) (int, error) {
	if !w.blockFlush {
		w.once.Do(func() { close(w.entered); <-w.unblock })

		if w.fail {
			return 0, io.ErrClosedPipe
		}
	}

	return w.ResponseRecorder.Write(b)
}

func (w *blockingResponse) FlushError() error {
	if w.blockFlush {
		w.once.Do(func() { close(w.entered); <-w.unblock })

		if w.fail {
			return io.ErrClosedPipe
		}
	}

	w.Flush()

	return nil
}

func (w *blockingResponse) Unwrap() http.ResponseWriter { return w.ResponseRecorder }

func awaitServerPolls(t *testing.T, s *Server, count int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := s.polls.count()

		if n == count {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("poll admission did not reach %d", count)
}

func (f *servingFixture) requestState(t *testing.T) *tls.ConnectionState {
	t.Helper()

	certs := make([]*x509.Certificate, len(f.certificate.Certificate))
	for i, der := range f.certificate.Certificate {
		var err error

		certs[i], err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
	}

	return &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: certs, VerifiedChains: [][]*x509.Certificate{certs}}
}

func TestAdmissionHeldThroughWriteCompletion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flush, fail bool
		status      int
	}{
		{"write", false, false, http.StatusOK},
		{"flush", true, false, http.StatusOK},
		{"failed write", false, true, http.StatusOK},
		{"failed flush", true, true, http.StatusOK},
		{"no content flush", true, false, http.StatusNoContent},
		{"error write", false, false, http.StatusServiceUnavailable},
		{"error flush", true, false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				f.a.Server.Config.Limits.MaxPolls = 1
				configureFixtureAge(t, f, 2*wire.PollWait)
				f.a.Server.Config.Limits.MaxConcurrentWrites = 1
				other := *f
				other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
				otherState := other.requestState(t)
				handler := f.a.Server.Handler()
				r := httptest.NewRequest("GET", wire.SnapshotPath, nil)

				r.TLS = f.requestState(t)
				if tc.status != http.StatusOK {
					current, err := f.a.authority.Current()
					if err != nil {
						t.Fatal(err)
					}

					cursor := current.Sequence()
					if tc.status == http.StatusServiceUnavailable {
						cursor++
					}

					r.URL.RawQuery = fmt.Sprintf("after=%d", cursor)
				}

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: tc.flush, fail: tc.fail}

				unblock := sync.OnceFunc(func() { close(w.unblock) })
				defer unblock()

				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); handler.ServeHTTP(w, r) }()

				select {
				case <-w.entered:
				case <-time.After(wire.PollWait + time.Second):
					t.Fatal("response did not start")
				}

				// Use an immediate cursor so this also proves error responses retain admission.
				r = r.Clone(f.ctx)

				r.URL.RawQuery = ""
				for _, state := range []*tls.ConnectionState{r.TLS, otherState} {
					second := httptest.NewRecorder()
					request := r.Clone(f.ctx)
					request.TLS = state
					handler.ServeHTTP(second, request)
					responseBody(t, second.Result(), nil, http.StatusTooManyRequests)
				}

				wantWrites := 1
				if tc.status == http.StatusServiceUnavailable {
					wantWrites = 0
				}

				if got := len(f.a.Server.writes); got != wantWrites {
					t.Fatalf("write slots during response: %d, want %d", got, wantWrites)
				}

				unblock()

				aborted := <-done
				if tc.fail && aborted != http.ErrAbortHandler || !tc.fail && aborted != nil {
					t.Fatalf("response abort: %v", aborted)
				}

				if !tc.fail && w.Code != tc.status {
					t.Fatalf("response status: %d, want %d", w.Code, tc.status)
				}

				if len(f.a.Server.writes) != 0 {
					t.Fatal("write slot leaked")
				}

				third := httptest.NewRecorder()
				handler.ServeHTTP(third, r.Clone(f.ctx))

				if third.Code != 200 {
					t.Fatalf("admission leaked: %d", third.Code)
				}
			})
		})
	}
}

func TestHTTPPollAdmissionAndCancellation(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := newServingFixture(t)
			f.a.Server.Config.Limits.MaxPolls = limit
			other := *f
			other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
			handler := f.a.Server.Handler()

			current, err := f.a.authority.Current()
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			r := httptest.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s?after=%d", wire.SnapshotPath, current.Sequence()), nil)
			r.TLS = f.requestState(t)
			done := make(chan *httptest.ResponseRecorder, 1)

			go func() { w := httptest.NewRecorder(); handler.ServeHTTP(w, r); done <- w }()

			awaitServerPolls(t, f.a.Server, 1)

			if len(f.a.Server.writes) != 0 {
				t.Fatal("long poll consumed write slot")
			}

			for _, state := range []*tls.ConnectionState{r.TLS, other.requestState(t)} {
				request := httptest.NewRequest("GET", wire.SnapshotPath, nil)
				request.TLS = state
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)

				want := http.StatusTooManyRequests
				if state != r.TLS && limit == 2 {
					want = http.StatusOK
				}

				responseBody(t, w.Result(), nil, want)
			}

			cancel()

			select {
			case w := <-done:
				responseBody(t, w.Result(), nil, http.StatusServiceUnavailable)
			case <-time.After(5 * time.Second):
				t.Fatal("canceled poll did not return")
			}

			awaitServerPolls(t, f.a.Server, 0)

			for _, state := range []*tls.ConnectionState{r.TLS, other.requestState(t)} {
				request := httptest.NewRequest("GET", wire.SnapshotPath, nil)
				request.TLS = state
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				responseBody(t, w.Result(), nil, http.StatusOK)
			}
		})
	}
}

func TestTLSAdmissionSaturationSendsInternalError(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxConcurrentBootstrap = 1

	endpoint := f.start(t)
	if !take(f.a.Server.authSlots) {
		t.Fatal("could not saturate admission")
	}
	defer release(f.a.Server.authSlots)

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(endpoint, "https://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	client := tls.Client(conn, &tls.Config{RootCAs: f.roots, ServerName: "example.com", MinVersion: tls.VersionTLS13})

	err = client.HandshakeContext(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("saturated ClientHello should send TLS internal_error, got %v", err)
	}
}

func TestAdmissionAndAPIDeadlines(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxConcurrentBootstrap = 1
	f.a.Server.Config.Limits.WriteTimeout = 100 * time.Millisecond
	entered := make(chan struct{}, 1)
	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
		entered <- struct{}{}

		<-ctx.Done()

		return ctx.Err()
	}})
	handler := f.a.Server.Handler()

	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", wire.BootstrapPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.TLS = f.requestState(t)
	done := make(chan *httptest.ResponseRecorder, 1)

	go func() { w := httptest.NewRecorder(); handler.ServeHTTP(w, r); done <- w }()

	<-entered

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r.Clone(f.ctx))

	if w.Code != 429 {
		t.Fatalf("auth concurrency unbounded: %d", w.Code)
	}

	select {
	case w := <-done:
		if w.Code != 503 {
			t.Fatalf("deadline: %d", w.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("API deadline not enforced")
	}
}
