// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestHTTPSBootstrapSnapshotAndStrictRoutes(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	anonymous := f.client(t, nil)

	req := bootstrapTestRequest(t, f.ctx, endpoint, f.token, f.request)
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

	req := bootstrapTestRequest(t, f.ctx, endpoint, f.token, f.request)
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
	req := bootstrapTestRequest(t, f.ctx, endpoint, f.token, f.request)
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
				require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node))

				if scenario == "excluded" {
					node.Labels = map[string]string{wire.ExclusionLabel: ""}
				} else {
					node.UID = "replacement"
				}

				require.NoError(t, f.a.Topology.Update(f.ctx, node))
			case "pod gone":
				pod := &corev1.Pod{}
				require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod))
				require.NoError(t, f.a.Topology.Delete(f.ctx, pod))
			case "expired":
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				require.NoError(t, err)

				time.Sleep(time.Until(leaf.NotAfter) + 10*time.Millisecond)
			}

			reused := false
			ctx := httptrace.WithClientTrace(f.ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

			req, err := http.NewRequestWithContext(ctx, "GET", endpoint+wire.SnapshotPath, nil)
			require.NoError(t, err)

			response, err = c.Do(req)

			want := 200

			switch scenario {
			case "expired":
				want = 401
			}

			responseBody(t, response, err, want)

			require.True(t, reused, "test failed to reuse TLS connection")
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

func keyringRequest(t *testing.T, f *servingFixture, bearer bool, query string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(f.ctx, http.MethodGet, wire.KeyringPath+query, nil)
	if bearer {
		r.TLS = &tls.ConnectionState{HandshakeComplete: true}
		r.Header.Set("Authorization", "Bearer "+f.token)
	} else {
		r.TLS = f.requestState(t)
	}

	return r
}

func requireKeyringResponse(t *testing.T, w *httptest.ResponseRecorder, status int) []byte {
	t.Helper()

	body := responseBody(t, w.Result(), nil, status)
	if w.Header().Get("Cache-Control") != "no-store" || !w.Flushed {
		t.Fatal("response not flushed or cacheable")
	}

	if len(body) > wire.MaxBundleBytes {
		t.Fatal("unbounded keyring response")
	}

	return body
}

func TestHTTPSKeyringAuthenticationAndEncoding(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	_, bundle, _, material := keyState(t, f.a.Keyring)

	expected, err := wire.EncodeBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	for _, bearer := range []bool{true, false} {
		r, err := http.NewRequestWithContext(f.ctx, http.MethodGet, endpoint+wire.KeyringPath, nil)
		if err != nil {
			t.Fatal(err)
		}

		peer := f.client(t, &f.certificate)
		if bearer {
			peer = f.client(t, nil)
			r.Header.Set("Authorization", "Bearer "+f.token)
		}

		response, err := peer.Do(r)

		body := responseBody(t, response, err, http.StatusOK)
		if !bytes.Equal(body, expected) || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json" {
			t.Fatal("response was not the full committed wire bundle")
		}

		for _, key := range material.Keys {
			if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(key.PrivateKey))) {
				t.Fatal("issuer private key disclosed")
			}
		}
	}
}

func TestKeyringRequestValidation(t *testing.T) {
	f := newServingFixture(t)
	handler := f.a.Server.Handler()

	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"anonymous", func(r *http.Request) { r.TLS = &tls.ConnectionState{HandshakeComplete: true} }, 401},
		{"no TLS", func(r *http.Request) { r.TLS = nil }, 401},
		{"both credentials", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }, 401},
		{"empty auth with certificate", func(r *http.Request) { r.Header["Authorization"] = []string{""} }, 401},
		{"duplicate bearer", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header["Authorization"] = []string{"Bearer " + f.token, "Bearer " + f.token}
		}, 401},
		{"bad bearer", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header.Set("Authorization", "Basic abc")
		}, 401},
		{"unverified certificate", func(r *http.Request) { r.TLS.VerifiedChains = nil }, 401},
		{"body", func(r *http.Request) { r.ContentLength = 1 }, 400},
		{"unknown body length", func(r *http.Request) { r.ContentLength = -1 }, 400},
		{"chunked", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, 400},
		{"encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 400},
		{"empty query", func(r *http.Request) { r.URL.ForceQuery = true }, 400},
		{"post", func(r *http.Request) { r.Method = http.MethodPost }, 400},
		{"head", func(r *http.Request) { r.Method = http.MethodHead }, 400},
		{"slash", func(r *http.Request) { r.URL.Path += "/" }, 400},
		{"escaped path", func(r *http.Request) { r.URL.RawPath = "/v1/%6beyring" }, 400},
		{"header limit", func(r *http.Request) {
			r.Header.Set("X-Large", strings.Repeat("x", f.a.Server.config.Limits.HeaderBytes))
		}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := keyringRequest(t, f, false, "")
			tc.mutate(r)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			requireKeyringResponse(t, w, tc.status)
		})
	}

	for _, query := range []string{"after=0", "after=999", "after=01", "after=+1", "after=%31", "after=1&after=1", "after=1&x=2", "other=1", "after=", "after=-1", "after=18446744073709551616"} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, false, "?"+query))

			status := 400
			if query == "after=0" || query == "after=999" {
				status = 409
			}

			requireKeyringResponse(t, w, status)
		})
	}
}

func TestKeyringBearerLiveBindings(t *testing.T) {
	for _, scenario := range []string{"token", "audience", "pod", "service account", "daemonset", "node", "API outage"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			status := http.StatusForbidden

			var obj client.Object

			key := client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}

			switch scenario {
			case "token", "audience":
				status = http.StatusUnauthorized
				fixtureDependencies[f.a.authority].Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					review := obj.(*authv1.TokenReview)
					review.Status.Authenticated = scenario == "audience"
					review.Status.Audiences = []string{"wrong-audience"}

					return nil
				}})
			case "pod":
				obj, key.Name = &corev1.Pod{}, "worker-pod"
			case "service account":
				obj = &corev1.ServiceAccount{}
			case "daemonset":
				obj = &appsv1.DaemonSet{}
			case "node":
				obj, key = &corev1.Node{}, client.ObjectKey{Name: "worker"}
			case "API outage":
				status = http.StatusServiceUnavailable
				fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("offline")
				}})
			}

			if obj != nil {
				if err := f.a.Topology.Get(f.ctx, key, obj); err != nil {
					t.Fatal(err)
				}

				if scenario == "node" {
					obj.SetLabels(map[string]string{wire.ExclusionLabel: ""})
				} else {
					obj.SetUID("recreated")
				}

				if err := f.a.Topology.Update(f.ctx, obj); err != nil {
					t.Fatal(err)
				}
			}

			w := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
			requireKeyringResponse(t, w, status)
		})
	}
}

func TestKeyringMTLSNeverReadsAPI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)

		var calls atomic.Int64

		unavailable := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				calls.Add(1)
				return errors.New("offline")
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				calls.Add(1)
				return errors.New("offline")
			},
		})
		fixtureDependencies[f.a.authority].Client, fixtureDependencies[f.a.authority].reader = unavailable, unavailable

		if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
			t.Fatal("outage hidden")
		}

		calls.Store(0)

		handler := f.a.Server.Handler()

		for _, query := range []string{"", "?after=1"} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, false, query))

			want := 200
			if query != "" {
				// Without background confirmation the replica expires during the poll.
				want = 503
			}

			body := requireKeyringResponse(t, w, want)
			if want == 204 && len(body) != 0 {
				t.Fatal("204 body")
			}
		}

		if calls.Load() != 0 {
			t.Fatal("mTLS keyring used API")
		}
	})
}

func TestKeyringPollWakeAndTermination(t *testing.T) {
	for _, bearer := range []bool{false, true} {
		for _, scenario := range []string{"timeout", "rotation", "invalidation", "leader canceled", "request canceled", "expired", "bearer revoked", "retired trust"} {
			t.Run(fmt.Sprintf("bearer=%t/%s", bearer, scenario), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { testKeyringPollTermination(t, bearer, scenario) })
			})
		}
	}
}

func testKeyringPollTermination(t *testing.T, bearer bool, scenario string) {
	t.Helper()
	f := newServingFixture(t)
	// Isolate poll termination from the default 30-second freshness gate.
	configureFixtureAge(t, f, time.Minute)

	if scenario == "expired" {
		f.expireIdentitySoon(t, bearer)
	}

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	r := keyringRequest(t, f, bearer, "?after=1").WithContext(ctx)
	handler := f.a.Server.Handler()
	w := httptest.NewRecorder()
	done := make(chan struct{})

	go func() { defer close(done); handler.ServeHTTP(w, r) }()

	synctest.Wait()

	polls := f.a.Server.keyringPolls.count()

	require.Equal(t, 1, polls)
	require.Empty(t, f.a.Server.writes)
	require.Empty(t, f.a.Server.bootstrapSlots)

	want := terminateKeyringPoll(t, f, cancel, bearer, scenario)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not wake")
	}

	body := requireKeyringResponse(t, w, want)
	if want == 200 {
		bundle, err := wire.DecodeBundle(bytes.NewReader(body))
		require.NoError(t, err)
		require.Equal(t, wire.Generation(2), bundle.Generation)
	}

	require.Zero(t, f.a.Server.keyringPolls.count(), "admission leaked")
}

func terminateKeyringPoll(t *testing.T, f *servingFixture, cancel context.CancelFunc, bearer bool, scenario string) int {
	t.Helper()

	want := 503

	switch scenario {
	case "timeout":
		want = 204
		// Repeated unchanged reconciliations must not extend the wait.
		time.Sleep(20 * time.Second)
		runKeys(t, f.a.Keyring)
		time.Sleep(10 * time.Second)
	case "expired":
		want = 401

		time.Sleep(time.Second)
	case "rotation":
		want = 200
		_, _, rotation, _ := keyState(t, f.a.Keyring)
		fixtureDependencies[f.a.authority].now = func() time.Time { return rotation.NextRotation }
		runKeys(t, f.a.Keyring)
	case "invalidation":
		invalidateFixtureTrust(t, f)
	case "leader canceled":
		f.cancel()
	case "request canceled":
		cancel()
	case "bearer revoked":
		want = 204
		if bearer {
			want = 403
		}

		pod := &corev1.Pod{}
		require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod))
		require.NoError(t, f.a.Topology.Delete(f.ctx, pod))

		time.Sleep(wire.PollWait)
	case "retired trust":
		want = 401
		if bearer {
			want = 200
		}

		replaceFixtureCredentials(t, f)
	}

	return want
}

func (f *servingFixture) expireIdentitySoon(t *testing.T, bearer bool) {
	t.Helper()

	if !bearer {
		f.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(time.Second) })
		return
	}

	_, status, _ := authFixture(t)
	f.token = "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Second).Unix())) + ".signature"
	installReview(t, f.a, status, f.token)
}

func TestKeyringAdmissionHeldThroughResponse(t *testing.T) {
	for _, flush := range []bool{false, true} {
		for _, status := range []int{200, 204, 409} {
			t.Run(fmt.Sprintf("flush=%t/status=%d", flush, status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					f.configureServer(func(c *Config) { c.Limits.MaxPolls = 1 })
					// Keep authority fresh through the 204 wait and blocked response.
					configureFixtureAge(t, f, time.Minute)
					handler := f.a.Server.Handler()

					query := ""
					if status == 204 {
						query = "?after=1"
					}

					if status == 409 {
						query = "?after=2"
					}

					w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: flush}
					// A 204 has no Write, only Flush.
					if status == 204 {
						w.blockFlush = true
					}

					unblock := sync.OnceFunc(func() { close(w.unblock) })
					defer unblock()

					done := make(chan struct{})
					r := keyringRequest(t, f, false, query)

					go func() { defer close(done); handler.ServeHTTP(w, r) }()

					<-w.entered

					other := *f

					other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
					for _, fixture := range []*servingFixture{f, &other} {
						second := httptest.NewRecorder()
						handler.ServeHTTP(second, keyringRequest(t, fixture, false, ""))
						requireKeyringResponse(t, second, 429)
					}
					// Independent snapshot admission is available while delivery blocks.
					require.True(t, f.a.Server.polls.acquire(wire.NodeID(testNodeUID)), "keyring consumed snapshot admission")

					f.a.Server.polls.release(wire.NodeID(testNodeUID))
					unblock()
					<-done
					requireKeyringResponse(t, w.ResponseRecorder, status)

					requireNoAdmission(t, f.a.Server)
				})
			})
		}
	}
}

func TestKeyringBearerAdmissionAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		f.configureServer(func(c *Config) {
			c.Limits.MaxConcurrentBootstrap = 1
			c.Limits.WriteTimeout = time.Second
		})
		s := f.a.Server
		fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		handler := s.Handler()
		w := httptest.NewRecorder()
		r := keyringRequest(t, f, true, "")
		done := make(chan struct{})

		go func() { defer close(done); handler.ServeHTTP(w, r) }()

		synctest.Wait()

		if len(s.bootstrapSlots) != 1 {
			t.Fatal("bearer API work not admitted")
		}

		second := httptest.NewRecorder()
		handler.ServeHTTP(second, keyringRequest(t, f, true, ""))
		requireKeyringResponse(t, second, 429)

		local := httptest.NewRecorder()
		handler.ServeHTTP(local, keyringRequest(t, f, false, ""))
		requireKeyringResponse(t, local, 200)
		time.Sleep(time.Second)
		<-done
		requireKeyringResponse(t, w, 503)

		if len(s.bootstrapSlots) != 0 || s.keyringPolls.count() != 0 {
			t.Fatal("API timeout leaked admission")
		}
	})
}

func TestKeyringPerNodeAdmissionIndependentOfSnapshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		f.configureServer(func(c *Config) { c.Limits.MaxPolls = 2 })
		handler := f.a.Server.Handler()

		ctx, cancel := context.WithCancel(f.ctx)
		defer cancel()

		request := keyringRequest(t, f, false, "?after=1").WithContext(ctx)
		done := make(chan struct{})

		go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), request) }()

		synctest.Wait()

		for _, bearer := range []bool{true, false} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, keyringRequest(t, f, bearer, ""))
			requireKeyringResponse(t, w, 429)
		}

		other := *f
		other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, keyringRequest(t, &other, false, ""))
		requireKeyringResponse(t, w, 200)
		w = httptest.NewRecorder()
		snapshot := keyringRequest(t, f, false, "")
		snapshot.URL.Path = wire.SnapshotPath
		handler.ServeHTTP(w, snapshot)
		responseBody(t, w.Result(), nil, 200)
		cancel()
		<-done
	})
}

func TestKeyringBearerExpiresDuringReauthentication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		f.expireIdentitySoon(t, true)

		reads := 0
		fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				reads++
				if reads == 2 {
					<-ctx.Done()
					return ctx.Err()
				}
			}

			return c.Get(ctx, key, obj, opts...)
		}})
		w := httptest.NewRecorder()
		f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
		requireKeyringResponse(t, w, 401)

		if reads != 2 {
			t.Fatal("did not exercise post-wait expiration")
		}
	})
}

// TestRustKeyringInterop runs the production Rust transport against the real Go
// HTTPS handler, issuer, and keyring reconciler with the existing fake API fixture.
// Opt in with RACER_RUST_INTEROP=1; no Kubernetes cluster is contacted.
func TestRustKeyringInterop(t *testing.T) {
	if os.Getenv("RACER_RUST_INTEROP") != "1" {
		t.Skip("set RACER_RUST_INTEROP=1 to run the Rust client")
	}

	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o700))
	directory, err := os.MkdirTemp(filepath.Join(root, "tmp"), "keyring-interop-")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	f := newServingFixture(t)

	cache := catalogCache("interop", testOtherUID)
	require.NoError(t, f.a.Topology.Create(f.ctx, &cache))

	runKeys(t, f.a.Keyring)
	reconcileTopology(t, f.a.Topology, f.ctx)
	endpoint := f.start(t)

	writeInteropConfig(t, f, directory, endpoint)

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "timeout", "--signal=TERM", "--kill-after=10s", "230s", "cargo", "test", "--locked", "--manifest-path", filepath.Join(root, "cmd/racer-dataplane/Cargo.toml"), "--test", "keyring_interop", "--", "--ignored", "--nocapture")

	cmd.Env = append(os.Environ(), "RACER_KEYRING_INTEROP_DIR="+directory)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	reaped := false

	defer func() {
		if !reaped {
			cancel()
			<-done
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	// No manager runs in this fixture. Keep authority observations fresh through
	// Cargo startup and the full no-change poll without relaxing freshness gates.
	refresh := time.NewTicker(time.Second)
	defer refresh.Stop()

	rotated := false

	for {
		select {
		case err := <-done:
			reaped = true

			require.NoError(t, err, "Rust interoperability client")
			require.True(t, rotated, "Rust client did not reach the rotation poll")

			return
		case <-refresh.C:
			runKeys(t, f.a.Keyring)
			reconcileTopology(t, f.a.Topology, f.ctx)
		case <-ticker.C:
			if rotated {
				continue
			}

			if _, err := os.Stat(filepath.Join(directory, "rotate")); err != nil {
				continue
			}

			parked := f.a.Server.keyringPolls.count() == 1

			if parked {
				_, _, rotation, _ := keyState(t, f.a.Keyring)
				fixtureDependencies[f.a.authority].now = func() time.Time { return rotation.NextRotation }
				runKeys(t, f.a.Keyring)

				rotated = true
			}
		}
	}
}

func writeInteropConfig(t *testing.T, f *servingFixture, directory, endpoint string) {
	t.Helper()

	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.serverCertificate.Certificate[0]})
	require.NoError(t, os.WriteFile(filepath.Join(directory, "trust.pem"), trust, 0o600))

	config, err := json.Marshal(map[string]string{
		"endpoint": endpoint, "cluster": string(f.a.Topology.Config.Cluster),
		"node": testNodeUID, "token": f.token,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "config.json"), config, 0o600))
}

func TestAuthenticatedRDMANICProposalAndEmptyRemoval(t *testing.T) {
	f := newServingFixture(t)
	f.request.Shares = 9
	f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_1", Port: 1, Rail: 0}, {Device: "mlx5_0", Port: 2, Rail: 0}}
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.token)
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.NoError(t, err)

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.Equal(t, "9", node.Annotations[enrolledSharesAnnotation])
	nics, err := wire.DecodeRDMANICs(strings.NewReader(node.Annotations[enrolledRDMANICsAnnotation]))
	require.NoError(t, err)
	require.Equal(t, wire.CanonicalRDMANICs(f.request.RDMANICs), nics)

	node.Annotations[wire.RDMANICsAnnotation] = "[]"
	require.NoError(t, f.a.Topology.Update(f.ctx, &node))
	attributes, err := members.ParseAnnotations(&node)
	require.NoError(t, err)
	require.Empty(t, attributes.RDMANICs)
	f.request.RDMANICs = nil
	f.request.Shares = 10
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.NoError(t, err)
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
	require.Equal(t, "10", node.Annotations[enrolledSharesAnnotation])
	require.Equal(t, "[]", node.Annotations[wire.RDMANICsAnnotation])
}

func TestEnrollmentRDMANICAtomicPatchFailure(t *testing.T) {
	f := newServingFixture(t)
	f.request.Shares = 9
	f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_0", Port: 1}}
	patchFailure := errors.New("patch rejected")
	patched := false
	fixtureDependencies[f.a.authority].Client = interceptor.NewClient(fixtureDependencies[f.a.authority].Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
			patched = true
			data, err := patch.Data(obj)
			require.NoError(t, err)
			require.Contains(t, string(data), enrolledSharesAnnotation)
			require.Contains(t, string(data), enrolledRDMANICsAnnotation)
			require.Contains(t, string(data), "resourceVersion")

			return patchFailure
		},
	})
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.token)
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.ErrorIs(t, err, patchFailure)
	require.True(t, patched)

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.NotContains(t, node.Annotations, enrolledSharesAnnotation)
	require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
}

func TestEnrollmentRDMANICLiveNodeRecheck(t *testing.T) {
	for _, scenario := range []string{"replacement UID", "excluded"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_0", Port: 1}}
			nodeReads := 0
			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}

					if node, ok := obj.(*corev1.Node); ok {
						nodeReads++
						if nodeReads > 1 {
							if scenario == "replacement UID" {
								node.UID = testOtherUID
							} else {
								node.Labels = map[string]string{wire.ExclusionLabel: ""}
							}
						}
					}

					return nil
				},
			})
			req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.token)
			_, err = f.a.Server.enroll(f.ctx, req, f.request)
			require.ErrorIs(t, err, wire.Forbidden)
			require.Equal(t, 2, nodeReads)

			var node corev1.Node
			require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
			require.NotContains(t, node.Annotations, enrolledSharesAnnotation)
			require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
		})
	}
}

func TestLocalTrustInvalidationDuringPoll(t *testing.T) {
	f := newServingFixture(t)

	current, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("%s?after=%d", wire.SnapshotPath, current.Sequence()), nil)
	req.TLS = f.requestState(t)
	handler := f.a.Server.Handler()
	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		done <- w
	}()

	deadline := time.After(5 * time.Second)

	for {
		n := f.a.Server.polls.count()

		if n == 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("poll did not park")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	withdrawServerTrust(t, f.a.Server)

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	_, _ = f.a.Topology.Reconcile(f.ctx, ctrl.Request{})

	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != `{"code":"unavailable"}` {
			t.Fatalf("invalidated trust disclosed publication: %d %s", w.Code, w.Body.String())
		}
	case <-deadline:
		t.Fatal("poll did not recheck local trust")
	}
}

func TestKeyringReauthenticationCannotDiscloseWithdrawnBundle(t *testing.T) {
	f := newServingFixture(t)
	reads := 0
	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			reads++
			if reads == 2 {
				invalidateFixtureTrust(t, f)
			}
		}

		return c.Get(ctx, key, obj, opts...)
	}})
	w := httptest.NewRecorder()
	f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
	requireKeyringResponse(t, w, 503)

	if reads != 2 {
		t.Fatal("bearer not rechecked before response")
	}
}

type completionResponse struct {
	*httptest.ResponseRecorder
	onWrite func()
	onFlush func()
}

func (w *completionResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if w.onWrite != nil {
		w.onWrite()
	}

	return n, err
}

func (w *completionResponse) Flush() {
	w.ResponseRecorder.Flush()

	if w.onFlush != nil {
		w.onFlush()
	}
}

func TestTrustAuthorityImmediateCompletion(t *testing.T) {
	for _, route := range []string{"bootstrap", "keyring", "keyring empty", "snapshot"} {
		for _, stage := range []string{"write", "flush"} {
			if route == "keyring empty" && stage == "write" {
				continue
			}

			for _, change := range []string{"invalidate", "recover", "rotate"} {
				t.Run(route+"/"+stage+"/"+change, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) { testTrustCompletion(t, route, stage, change) })
				})
			}
		}
	}
}

func testTrustCompletion(t *testing.T, route, stage, change string) {
	t.Helper()
	f := newServingFixture(t)
	s := f.a.Server

	request := f.publicRequest(t, route)
	called := false
	changeAuthority := func() {
		called = true

		if change != "rotate" {
			withdrawServerTrust(t, s)
		}

		if change == "recover" {
			restoreServerTrust(t, s)
		}

		if change == "rotate" {
			rotateFixtureTrust(t, f)
		}
		// Deliberately do not yield or wait for cancellation callbacks.
	}

	w := &completionResponse{ResponseRecorder: httptest.NewRecorder()}
	if stage == "write" {
		w.onWrite = changeAuthority
	} else {
		w.onFlush = changeAuthority
	}

	aborted := serveRecover(s.Handler(), w, request)
	require.True(t, called, "completion hook not reached: %d", w.Code)

	if change == "rotate" {
		require.Nil(t, aborted, "ordinary rotation aborted admitted response")
	} else {
		require.Equal(t, http.ErrAbortHandler, aborted, "revoked response completed")
	}

	requireNoAdmission(t, s)
}

func TestTrustAuthoritySynchronousRevocation(t *testing.T) {
	f := newServingFixture(t)

	guard, cancel, err := f.a.authority.AdmitTrust(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	withdrawServerTrust(t, f.a.Server)

	if guard.Check(t.Context()) != context.Canceled {
		t.Fatal("revocation depends on callback scheduling")
	}
}

func TestPublicBlockedWriteTrustAuthority(t *testing.T) {
	for _, route := range []string{"snapshot", "bootstrap", "keyring"} {
		for _, change := range []string{"invalidate", "invalidate recover", "expire", "reconfirm"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					s := f.a.Server
					configureFixtureAge(t, f, 5*time.Second)

					server, peer := net.Pipe()
					defer server.Close()
					defer peer.Close()

					request := f.publicRequest(t, route)
					request = request.WithContext(connectionContext(f.ctx, server))
					w := &pipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server}
					done := make(chan any, 1)

					go func() { defer func() { done <- recover() }(); s.Handler().ServeHTTP(w, request) }()

					synctest.Wait()

					require.Len(t, s.writes, 1, "response not blocked in write")

					start := time.Now()

					switch change {
					case "invalidate":
						withdrawServerTrust(t, s)
					case "invalidate recover":
						withdrawServerTrust(t, s)
						restoreServerTrust(t, s)
					case "expire":
						time.Sleep(3 * time.Second)
						reconcileTopology(t, f.a.Topology, f.ctx)
						time.Sleep(2 * time.Second)
					case "reconfirm":
						time.Sleep(3 * time.Second)

						_, err := f.a.authority.ReconcileCredentials(t.Context())
						require.NoError(t, err)

						reconcileTopology(t, f.a.Topology, f.ctx)

						time.Sleep(2 * time.Second)
					}

					require.Equal(t, http.ErrAbortHandler, <-done, "blocked response did not abort")

					want := time.Duration(0)
					if change == "expire" || change == "reconfirm" {
						want = 5 * time.Second
					}

					require.Equal(t, want, time.Since(start), "trust cancellation delay")
					require.NoError(t, f.a.authority.PublicationReady(), "test must retain fresh publication independently of trust")
					require.Empty(t, s.writes)
					require.Empty(t, s.bootstrapSlots)

					if change == "invalidate recover" || change == "reconfirm" {
						require.NoError(t, f.a.authority.TrustReady(), "new requests should have usable trust")
					}
				})
			})
		}
	}
}

func TestTrustRotationKeepsAdmittedResponseBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		configureFixtureAge(t, f, 5*time.Second)

		guard, cancel, err := f.a.authority.AdmitTrust(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()

		ctx := guard.Context()
		deadline, _ := ctx.Deadline()

		time.Sleep(3 * time.Second)

		rotateFixtureTrust(t, f)

		if ctx.Err() != nil {
			t.Fatal("normal rotation revoked admitted response")
		}

		if got, _ := ctx.Deadline(); got != deadline {
			t.Fatal("rotation extended admitted freshness")
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()

		if ctx.Err() == nil {
			t.Fatal("admitted trust outlived pinned freshness")
		}

		if err := f.a.authority.TrustReady(); err != nil {
			t.Fatal("rotation should admit fresh requests", err)
		}
	})
}

func eventually(t *testing.T, description string, ready func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}

		time.Sleep(time.Millisecond)
	}
}

func TestReplicationRouteAuthorizationAndEarlyListener(t *testing.T) {
	f := newServingFixture(t)
	r := f.a.Replication
	r.Config.ControllerServiceAccount = "racer-controller"
	r.leader = f.ctx
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controller"}}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "racer-controller", UID: "controller-sa"}}
	for _, obj := range []client.Object{pod, sa} {
		require.NoError(t, r.Client.Create(f.ctx, obj))
	}

	username := "system:serviceaccount:" + r.Config.Namespace + ":racer-controller"
	audience := ReplicationAudience
	r.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review := obj.(*authv1.TokenReview)
		require.Equal(t, []string{ReplicationAudience}, review.Spec.Audiences, "wrong review audience")

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience}, User: authv1.UserInfo{Username: username, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})
	fixtureDependencies[r.authority].Client = r.Client

	for _, unchanged := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocked flush unchanged=%v failure=%v", unchanged, fail), func(t *testing.T) {
				testReplicationFlush(t, f, unchanged, fail)
			})
		}
	}

	for _, tc := range []struct {
		name string
		code int
	}{{"controller", 200}, {"duplicate poll", 429}, {"dataplane", 403}, {"wrong audience", 401}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "duplicate poll" {
				require.True(t, f.a.Server.replicationPolls.acquire(string(pod.UID)), "could not reserve replication poll")
				defer f.a.Server.replicationPolls.release(string(pod.UID))
			}

			if tc.name == "dataplane" {
				username = "system:serviceaccount:" + r.Config.Namespace + ":racer-dataplane"
			}

			if tc.name == "wrong audience" {
				audience = wire.TokenAudience
			}

			request := httptest.NewRequest(http.MethodGet, ReplicationPath, nil)
			request.TLS = f.requestState(t)
			request.Header.Set("Authorization", "Bearer "+f.token)

			response := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(response, request)

			require.Equal(t, tc.code, response.Code, response.Body.String())
		})
	}
	// TLS must be reachable before public readiness, including before trust is
	// initialized. Public routes remain unavailable; internal auth is independent.
	invalidateFixtureTrust(t, f)
	endpoint := f.start(t)
	peer := f.client(t, nil)
	response, err := peer.Get(endpoint + ReplicationPath)
	responseBody(t, response, err, http.StatusUnauthorized)
}

func TestSnapshotBlockedWriteClosesAtPinnedFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		configureFixtureAge(t, f, 5*time.Second)

		server, peer := net.Pipe()
		defer server.Close()
		defer peer.Close()

		request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
		request.TLS = f.requestState(t)
		request = request.WithContext(connectionContext(f.ctx, server))
		w := &pipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server}
		done := make(chan any, 1)

		go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

		synctest.Wait()
		time.Sleep(3 * time.Second)

		if err := f.a.authority.Observe(f.ctx); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Second)

		if aborted := <-done; aborted != http.ErrAbortHandler {
			t.Fatalf("blocked write did not abort: %v", aborted)
		}

		if len(f.a.Server.writes) != 0 {
			t.Fatal("blocked write retained admission")
		}

		if f.a.authority.PublicationReady() != nil {
			t.Fatal("confirmation should allow a new request")
		}
	})
}

type pipeResponse struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w *pipeResponse) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func TestRevokedDeltaCannotBorrowNewAuthority(t *testing.T) {
	r := initializedTopology(t)
	membership := AcceptedMembers{}

	for i := range 100 {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		membership[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}
	}

	base := replicationSmokePublish(t, t.Context(), r, membership)
	id := wire.NodeID("22222222-2222-4222-8222-000000000000")
	member := membership[id]
	member.Shares++
	membership[id] = member
	p := replicationSmokePublish(t, t.Context(), r, membership)
	delta := p.handle.ForBase(base.record.Sequence, base.record.ContentHash)

	guard, cancel, err := p.admit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	var encoded bytes.Buffer

	_, err = delta.WriteTo(t.Context(), guard, &encoded)
	require.NoError(t, err)
	require.Less(t, encoded.Len(), len(p.encoded), "test must exercise the real delta response")
	baseImage, err := wire.DecodePublication(strings.NewReader(base.encoded))
	require.NoError(t, err)
	_, err = wire.ApplyDelta(baseImage, &encoded)
	require.NoError(t, err)

	restore := withdrawPublication(t, r)
	restore()
	reconcileTopology(t, r, t.Context())

	<-guard.Context().Done()

	if _, err := delta.WriteTo(t.Context(), guard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("revoked delta: %v", err)
	}

	if _, _, err := p.admit(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("old image borrowed new authority: %v", err)
	}
}

func TestSnapshotAuthorityHeldThroughFlush(t *testing.T) {
	for _, action := range []string{"suspend", "freshness", "advance"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				configureFixtureAge(t, f, 5*time.Second)

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true}
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				<-w.entered

				_, err := f.a.authority.Current()
				require.NoError(t, err)

				switch action {
				case "advance":
					replicationSmokePublish(t, f.ctx, f.a.Topology, AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 9, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}})
				case "suspend":
					restore := withdrawPublication(t, f.a.Topology)
					restore()
					reconcileTopology(t, f.a.Topology, f.ctx)
				default:
					time.Sleep(3 * time.Second)

					require.NoError(t, f.a.authority.Observe(f.ctx))

					time.Sleep(2 * time.Second)
				}

				require.Len(t, f.a.Server.writes, 1)
				require.Equal(t, 1, f.a.Server.polls.count())

				close(w.unblock)

				if action == "advance" {
					require.Nil(t, <-done, "ordinary advancement aborted admitted flush")
				} else {
					require.Equal(t, http.ErrAbortHandler, <-done, "revoked flush completed")
				}

				require.Empty(t, f.a.Server.writes)
				require.Zero(t, f.a.Server.polls.count())
			})
		})
	}
}

func TestSnapshotDeltaRequiresSequenceAndHash(t *testing.T) {
	f := newServingFixture(t)
	membership := AcceptedMembers{}

	for i := range 100 {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		membership[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}
	}

	base := replicationSmokePublish(t, f.ctx, f.a.Topology, membership)
	id := wire.NodeID("22222222-2222-4222-8222-000000000000")
	member := membership[id]
	member.Shares++
	membership[id] = member
	next := replicationSmokePublish(t, f.ctx, f.a.Topology, membership)
	baseImage, err := wire.DecodePublication(strings.NewReader(base.encoded))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, query, hash string
		delta             bool
	}{
		{"exact", fmt.Sprintf("?after=%d", base.record.Sequence), base.record.ContentHash, true},
		{"older sequence same hash", fmt.Sprintf("?after=%d", base.record.Sequence-1), base.record.ContentHash, false},
		{"no sequence same hash", "", base.record.ContentHash, false},
		{"wrong hash", fmt.Sprintf("?after=%d", base.record.Sequence), strings.Repeat("a", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, wire.SnapshotPath+tc.query, nil)
			r.TLS = f.requestState(t)
			r.Header.Set(wire.DeltaHeader, tc.hash)

			w := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(w, r)
			require.Equal(t, http.StatusOK, w.Code)

			if tc.delta {
				applied, err := wire.ApplyDelta(baseImage, bytes.NewReader(w.Body.Bytes()))
				require.NoError(t, err)
				require.Equal(t, next.record.Sequence, applied.Sequence)
				require.Less(t, w.Body.Len(), len(next.encoded))
			} else {
				require.Equal(t, next.encoded, w.Body.String())
			}

			requireNoAdmission(t, f.a.Server)
		})
	}
}

func (w *pipeResponse) Write(b []byte) (int, error) { return w.conn.Write(b) }

func authFixture(t *testing.T) (*Application, authv1.TokenReviewStatus, string) {
	t.Helper()

	controller := true
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "ds-uid"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "sa-uid"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID(testNodeUID)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "worker-pod", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name}, Status: corev1.PodStatus{PodIP: "192.0.2.1"}}
	r := initializedTopology(t, ds, sa, node, pod)
	a := assembleFixture(r.Config, r.Client, r.APIReader)
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:racer:racer-dataplane", UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}, "authentication.kubernetes.io/node-name": {node.Name}, "authentication.kubernetes.io/node-uid": {string(node.UID)}}}}
	token := "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".signature"

	return a, status, token
}

func installReview(t *testing.T, a *Application, status authv1.TokenReviewStatus, token string) {
	t.Helper()

	fixtureDependencies[a.authority].Client = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return c.Create(ctx, obj, opts...)
		}

		if review.Spec.Token != token || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != wire.TokenAudience {
			t.Error("TokenReview did not bind token/audience")
		}

		review.Status = *status.DeepCopy()

		return ctx.Err()
	}})
}

func TestBootstrapRequiresUnambiguousNodeBindings(t *testing.T) {
	for _, key := range []string{"node-name", "node-uid"} {
		for _, values := range []authv1.ExtraValue{nil, {}, {""}, {"wrong"}, {"worker", "worker"}, {testNodeUID, testNodeUID}} {
			t.Run(fmt.Sprintf("%s/%v", key, values), func(t *testing.T) {
				a, status, token := authFixture(t)
				fullKey := "authentication.kubernetes.io/" + key
				delete(status.User.Extra, fullKey)

				if values != nil {
					status.User.Extra[fullKey] = values
				}

				installReview(t, a, status, token)

				r := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
				r.Header.Set("Authorization", "Bearer "+token)

				if _, err := a.authority.Authenticate(t.Context(), r); err == nil {
					t.Fatal("accepted missing or ambiguous node binding")
				}
			})
		}
	}
}

func TestBootstrapAuthoritativeBindings(t *testing.T) {
	for _, scenario := range []string{"success", "audience", "not authenticated", "review error", "username", "sa uid", "pod uid", "missing bound pod", "ambiguous bound pod", "node extra", "node extra uid", "recreated pod", "recreated sa", "recreated ds", "owner name", "owner kind", "owner not controller", "pod sa", "unscheduled", "terminal pod", "excluded node", "deleted node", "api failure", "canceled", "expired token", "duplicate bearer"} {
		t.Run(scenario, func(t *testing.T) {
			a, status, token := authFixture(t)
			runKeys(t, a.Keyring)
			reconcileTopology(t, a.Topology, t.Context())
			a.Lifecycle.process, a.Lifecycle.synced = t.Context(), true
			a.Lifecycle.SetServingReady(true)
			a.Server.tlsConfig(t.Context(), servingTestCertificate(t, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), nil, false))
			_, enrollment, _ := issuanceRequest(t, a.Keyring)
			enrollment.RDMANICs = []wire.RDMANIC{{Device: "mlx5_0", Port: 1}}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			pod := &corev1.Pod{}
			require.NoError(t, a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod))

			mutateBootstrapReview(scenario, &status)
			mutateBootstrapPod(scenario, pod)

			switch scenario {
			case "recreated sa", "recreated ds":
				var obj client.Object = &corev1.ServiceAccount{}
				if scenario == "recreated ds" {
					obj = &appsv1.DaemonSet{}
				}

				require.NoError(t, a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}, obj))

				obj.SetUID("replacement")

				require.NoError(t, a.Topology.Update(ctx, obj))
			case "terminal pod":
				pod.Status.Phase = corev1.PodFailed
				require.NoError(t, a.Topology.Client.Status().Update(ctx, pod))
			case "excluded node", "deleted node":
				node := &corev1.Node{}
				require.NoError(t, a.Topology.Get(ctx, client.ObjectKey{Name: "worker"}, node))

				if scenario == "deleted node" {
					require.NoError(t, a.Topology.Delete(ctx, node))
				} else {
					node.Labels = map[string]string{wire.ExclusionLabel: ""}
					require.NoError(t, a.Topology.Update(ctx, node))
				}
			case "expired token":
				token = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			}

			require.NoError(t, a.Topology.Update(ctx, pod))

			installReview(t, a, status, token)

			if scenario == "api failure" {
				fixtureDependencies[a.authority].reader = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return fmt.Errorf("private upstream failure")
				}})
			}

			if scenario == "canceled" {
				cancel()
			}

			req := bootstrapTestRequest(t, ctx, "", token, enrollment)
			req.TLS = &tls.ConnectionState{HandshakeComplete: true}

			if scenario == "duplicate bearer" {
				req.Header.Add("Authorization", "Bearer "+token)
			}

			w := httptest.NewRecorder()
			a.Server.Handler().ServeHTTP(w, req)

			requireBootstrapBindingResult(t, a, scenario, w)
		})
	}
}

func requireBootstrapBindingResult(t *testing.T, a *Application, scenario string, w *httptest.ResponseRecorder) {
	t.Helper()

	if scenario == "success" {
		issued := decodeIssuedResponse(t, responseBody(t, w.Result(), nil, http.StatusOK))
		leaf, err := x509.ParseCertificate(issued.CertificateChain[0])
		require.NoError(t, err)
		require.Equal(t, wire.NodeID(testNodeUID), issued.Node)
		require.Equal(t, a.Topology.Config.Cluster, issued.Cluster)
		require.True(t, leaf.NotAfter.After(time.Now()))

		return
	}

	want := http.StatusForbidden

	switch scenario {
	case "audience", "not authenticated", "review error", "missing bound pod", "ambiguous bound pod", "expired token", "duplicate bearer":
		want = http.StatusUnauthorized
	case "api failure", "canceled":
		want = http.StatusServiceUnavailable
	}

	responseBody(t, w.Result(), nil, want)

	if scenario != "deleted node" {
		var node corev1.Node
		require.NoError(t, a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
		require.Empty(t, node.Annotations[enrolledSharesAnnotation], "rejected enrollment persisted shares")
		require.Empty(t, node.Annotations[enrolledRDMANICsAnnotation], "rejected enrollment persisted NICs")
	}
}

func mutateBootstrapReview(scenario string, status *authv1.TokenReviewStatus) {
	switch scenario {
	case "audience":
		status.Audiences = []string{"api"}
	case "not authenticated":
		status.Authenticated = false
	case "review error":
		status.Error = "private upstream details"
	case "username":
		status.User.Username = "system:serviceaccount:other:racer-dataplane"
	case "sa uid":
		status.User.UID = "old-sa"
	case "pod uid":
		status.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"old-pod"}
	case "missing bound pod":
		delete(status.User.Extra, "authentication.kubernetes.io/pod-name")
	case "ambiguous bound pod":
		status.User.Extra["authentication.kubernetes.io/pod-name"] = authv1.ExtraValue{"worker-pod", "other"}
	case "node extra":
		status.User.Extra["authentication.kubernetes.io/node-name"] = authv1.ExtraValue{"other"}
	case "node extra uid":
		status.User.Extra["authentication.kubernetes.io/node-uid"] = authv1.ExtraValue{"old"}
	}
}

func mutateBootstrapPod(scenario string, pod *corev1.Pod) {
	switch scenario {
	case "recreated pod":
		pod.UID = "replacement"
	case "owner name":
		pod.OwnerReferences[0].Name = "other"
	case "owner kind":
		pod.OwnerReferences[0].Kind = "ReplicaSet"
	case "owner not controller":
		pod.OwnerReferences[0].Controller = nil
	case "pod sa":
		pod.Spec.ServiceAccountName = "other"
	case "unscheduled":
		pod.Spec.NodeName = ""
	}
}

func TestEnrollmentSaturationPreservesLocalAuthentication(t *testing.T) {
	f := newServingFixture(t)
	f.configureServer(func(c *Config) { c.Limits.MaxConcurrentBootstrap = 1 })
	s := f.a.Server
	entered := make(chan struct{})
	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			close(entered)
			<-ctx.Done()

			return ctx.Err()
		},
	})
	endpoint := f.start(t)

	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	r := httptest.NewRequestWithContext(ctx, http.MethodPost, wire.BootstrapPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.TLS = &tls.ConnectionState{HandshakeComplete: true}
	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)

		done <- w
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("enrollment did not enter API wait")
	}

	// A fresh TLS connection and snapshot both succeed while enrollment is full.
	peer := f.client(t, &f.certificate)
	response, err := peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusOK)
	// Isolated admission must preserve certificate authentication.
	anonymous := f.client(t, nil)
	response, err = anonymous.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusUnauthorized)
	// New enrollment reaches HTTP backpressure without another API operation.
	response, err = anonymous.Post(endpoint+wire.BootstrapPath, "application/json", bytes.NewReader(body))
	responseBody(t, response, err, http.StatusTooManyRequests)

	if len(s.bootstrapSlots) != 1 || len(s.authSlots) != 0 {
		t.Fatal("enrollment consumed local authentication capacity")
	}

	// Saturation cannot bypass withdrawn trust, even on an established connection.
	fixtureDependencies[f.a.authority].reader = f.a.Topology.Client
	invalidateFixtureTrust(t, f)

	response, err = peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusServiceUnavailable)
	fresh := f.client(t, &f.certificate)

	response, err = fresh.Get(endpoint + wire.SnapshotPath)
	if err == nil {
		response.Body.Close()
		t.Fatal("fresh handshake accepted withdrawn trust")
	}

	cancel()

	select {
	case w := <-done:
		responseBody(t, w.Result(), nil, http.StatusServiceUnavailable)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled enrollment retained admission")
	}

	if len(s.bootstrapSlots) != 0 || len(s.authSlots) != 0 {
		t.Fatal("admission leaked after cancellation")
	}
}

func TestBootstrapIssuanceBeforeWriteAdmission(t *testing.T) {
	for _, scenario := range []string{"success", "write saturation", "readiness lost", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			f.configureServer(func(c *Config) { c.Limits.MaxConcurrentWrites = 1 })
			s := f.a.Server
			handler := s.Handler()

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			wantWrites, wantStatus := 0, http.StatusOK

			if scenario == "write saturation" {
				take(s.writes)
				defer release(s.writes)

				wantWrites, wantStatus = 1, http.StatusTooManyRequests
			} else if scenario != "success" {
				wantStatus = http.StatusServiceUnavailable
			}

			reads := 0
			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++

				if len(s.writes) != wantWrites || len(s.bootstrapSlots) != 1 || len(s.authSlots) != 0 {
					t.Error("issuance changed write/auth admission timing")
				}

				switch scenario {
				case "readiness lost":
					f.a.Lifecycle.SetServingReady(false)
				case "canceled":
					cancel()
				}

				return c.Get(ctx, key, obj, opts...)
			}})

			r := bootstrapTestRequest(t, ctx, "", f.token, f.request)
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			encoded := responseBody(t, w.Result(), nil, wantStatus)
			require.NotZero(t, reads, "issuance skipped")
			require.Empty(t, s.bootstrapSlots)
			require.Empty(t, s.authSlots)
			require.Len(t, s.writes, wantWrites)

			if scenario == "success" {
				response := decodeIssuedResponse(t, encoded)
				require.Equal(t, f.request.Enrollment, response.Enrollment)
				require.Equal(t, wire.NodeID(testNodeUID), response.Node)
				require.True(t, w.Flushed)
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			}
		})
	}
}
