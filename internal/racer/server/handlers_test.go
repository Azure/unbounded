// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
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
			r.Header.Set("X-Large", strings.Repeat("x", f.a.Server.Config.Limits.HeaderBytes))
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
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					// Isolate poll termination from the default 30-second freshness gate.
					configureFixtureAge(t, f, time.Minute)

					if scenario == "expired" {
						if bearer {
							_, status, _ := authFixture(t)
							f.token = "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Second).Unix())) + ".signature"
							installReview(t, f.a, status, f.token)
						} else {
							f.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(time.Second) })
						}
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

					if polls != 1 || len(f.a.Server.writes) != 0 || len(f.a.Server.bootstrapSlots) != 0 {
						t.Fatal("poll not parked independently of auth/write admission")
					}

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
						if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
							t.Fatal(err)
						}

						if err := f.a.Topology.Delete(f.ctx, pod); err != nil {
							t.Fatal(err)
						}

						time.Sleep(wire.PollWait)
					case "retired trust":
						want = 401
						if bearer {
							want = 200
						}

						replaceFixtureCredentials(t, f)
					}

					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("poll did not wake")
					}

					body := requireKeyringResponse(t, w, want)
					if want == 200 {
						bundle, err := wire.DecodeBundle(bytes.NewReader(body))
						if err != nil || bundle.Generation != 2 {
							t.Fatalf("updated bundle: %v", err)
						}
					}

					if f.a.Server.keyringPolls.count() != 0 {
						t.Fatal("admission leaked")
					}
				})
			})
		}
	}
}

func TestKeyringAdmissionHeldThroughResponse(t *testing.T) {
	for _, flush := range []bool{false, true} {
		for _, status := range []int{200, 204, 409} {
			t.Run(fmt.Sprintf("flush=%t/status=%d", flush, status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					f.a.Server.Config.Limits.MaxPolls = 1
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
					if !f.a.Server.admitPoll(wire.NodeID(testNodeUID)) {
						t.Fatal("keyring consumed snapshot admission")
					}

					f.a.Server.releasePoll(wire.NodeID(testNodeUID))
					unblock()
					<-done
					requireKeyringResponse(t, w.ResponseRecorder, status)

					if f.a.Server.keyringPolls.count() != 0 || len(f.a.Server.writes) != 0 {
						t.Fatal("admission leaked")
					}
				})
			})
		}
	}
}

func TestKeyringBearerAdmissionAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		s := f.a.Server
		s.Config.Limits.MaxConcurrentBootstrap = 1
		s.Config.Limits.WriteTimeout = time.Second
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
		f.a.Server.Config.Limits.MaxPolls = 2
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
		_, status, _ := authFixture(t)
		f.token = "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Second).Unix())) + ".signature"
		installReview(t, f.a, status, f.token)

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
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	directory, err := os.MkdirTemp(filepath.Join(root, "tmp"), "keyring-interop-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	f := newServingFixture(t)

	cache := catalogCache("interop", testOtherUID)
	if err := f.a.Topology.Create(f.ctx, &cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, f.a.Keyring)
	reconcileTopology(t, f.a.Topology, f.ctx)
	endpoint := f.start(t)

	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.serverCertificate.Certificate[0]})
	if err := os.WriteFile(filepath.Join(directory, "trust.pem"), trust, 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := json.Marshal(map[string]string{
		"endpoint": endpoint,
		"cluster":  string(f.a.Topology.Config.Cluster),
		"node":     testNodeUID,
		"token":    f.token,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(directory, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}

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

			if err != nil {
				t.Fatalf("Rust interoperability client: %v", err)
			}

			if !rotated {
				t.Fatal("Rust client did not reach the rotation poll")
			}

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

// These are real HTTPS protocol clients, not Rust processes or in-memory Wait
// calls. Kubernetes authority is fake. Never interpret this as API capacity.
func TestReplicatedServingSmoke(t *testing.T) { replicatedServingSmoke(t, 12) }

func TestReplicatedServingCapacity(t *testing.T) {
	value := os.Getenv("RACER_REPLICATION_CLIENTS")
	if value == "" {
		t.Skip("set RACER_REPLICATION_CLIENTS to an exact count in [1,10000]")
	}

	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 10_000 {
		t.Fatal("RACER_REPLICATION_CLIENTS must be in [1,10000]; 100k requires distributed validation, not this loopback harness")
	}

	replicatedServingSmoke(t, count)
}

type replicationSmokeListener struct {
	net.Listener
	accepted atomic.Int64
	live     atomic.Int64
}

type replicationSmokeConn struct {
	net.Conn
	once  sync.Once
	owner *replicationSmokeListener
}

func (l *replicationSmokeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	l.accepted.Add(1)
	l.live.Add(1)

	return &replicationSmokeConn{Conn: c, owner: l}, nil
}

func (c *replicationSmokeConn) Close() error {
	c.once.Do(func() { c.owner.live.Add(-1) })
	return c.Conn.Close()
}

type replicationSmokeReplica struct {
	a        *Application
	ctx      context.Context
	cancel   context.CancelFunc
	endpoint string
	listener *replicationSmokeListener
	done     chan error
}

type replicationSmokePeer struct {
	client  *http.Client
	replica int
}

type replicationSmokeResult struct {
	index   int
	bytes   int64
	digest  [32]byte
	elapsed time.Duration
	err     error
}

func replicatedServingSmoke(t *testing.T, count int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	f := newServingFixture(t)
	accepted := make(AcceptedMembers, count)
	peers := make([]replicationSmokePeer, count)
	handshakes := make(chan struct{}, 24)
	setup := time.Now()
	_, _, rotation, material := keyState(t, f.a.Keyring)

	ca, signingKey, err := parseSigning(material.Keys[rotation.ActiveIssuer])
	if err != nil {
		t.Fatal(err)
	}

	for i := range peers {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		accepted[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: fmt.Sprintf("10.%d.%d.%d:8082", i>>16, (i>>8)&255, i&255), RDMANICs: []wire.RDMANIC{}}

		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		template := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "spiffe", Host: string(f.request.Cluster), Path: "/node/" + string(id)}}}

		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, signingKey)
		if err != nil {
			t.Fatal(err)
		}

		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}}}, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second}
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			select {
			case handshakes <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			defer func() { <-handshakes }()

			bounded, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()

			return (&tls.Dialer{Config: transport.TLSClientConfig}).DialContext(bounded, network, address)
		}
		t.Cleanup(transport.CloseIdleConnections)
		peers[i] = replicationSmokePeer{client: &http.Client{Transport: transport, Timeout: 45 * time.Second}, replica: i % 3}
	}

	base := replicationSmokePublish(t, ctx, f.a.Topology, accepted)

	var replicas []*replicationSmokeReplica

	for range 3 {
		a := assembleFixture(f.a.Topology.Config, f.a.Topology.Client, f.a.Topology.APIReader)
		process, stop := context.WithCancel(ctx)
		a.authority.BindProcess(process)
		replicationSmokeLifecycle(a.Lifecycle, process)
		a.Replication.observe(process)

		listener, err := (&net.ListenConfig{}).Listen(process, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		r := &replicationSmokeReplica{a: a, ctx: process, cancel: stop, endpoint: "https://" + listener.Addr().String(), listener: &replicationSmokeListener{Listener: listener}, done: make(chan error, 1)}
		replicas = append(replicas, r)

		config := a.Server.tlsConfig(process, f.serverCertificate)

		go func() {
			r.done <- a.Server.serve(process, r.listener, config)
		}()

		t.Cleanup(func() {
			stop()

			select {
			case err := <-r.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(12 * time.Second):
				t.Error("replica shutdown exceeded bound")
			}
		})
	}

	// Test-local leader selection and TokenReview, but the production TLS route,
	// controller Pod/SA checks, bounded decoder and durable install are exercised.
	leader := replicas[0]
	leader.a.Replication.leader = leader.ctx

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: f.a.Topology.Config.Namespace, Name: f.a.Topology.Config.ControllerServiceAccount, UID: "smoke-controller-sa"}}
	if err := f.a.Topology.Create(ctx, sa); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]*corev1.Pod{}

	for i := 1; i < len(replicas); i++ {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sa.Namespace, Name: fmt.Sprintf("controller-%d", i), UID: types.UID(fmt.Sprintf("controller-uid-%d", i))}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name}}
		if err := f.a.Topology.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}

		tokens[f.token+strconv.Itoa(i)] = pod
	}

	var reviews atomic.Int64

	leader.a.Replication.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return fmt.Errorf("unexpected API create %T", obj)
		}

		reviews.Add(1)

		pod := tokens[review.Spec.Token]
		if pod == nil || !slices.Equal(review.Spec.Audiences, []string{ReplicationAudience}) {
			return nil
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{ReplicationAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:" + sa.Namespace + ":" + sa.Name, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})
	fixtureDependencies[leader.a.authority].Client = leader.a.Replication.Client

	// Initial publisher installation uses the same canonical/durable validation.
	image, err := wire.DecodePublication(strings.NewReader(base.encoded))
	if err != nil {
		t.Fatal(err)
	}

	if err := leader.a.Replication.installReplica(ctx, leader.ctx, image); err != nil {
		t.Fatal(err)
	}

	internal := f.client(t, nil)
	internal.Timeout = 30 * time.Second
	replicate := func(publication *CommittedPublication) {
		t.Helper()

		start := time.Now()

		for i, r := range replicas {
			if i == 0 || r.ctx.Err() != nil {
				continue
			}

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+ReplicationPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Authorization", "Bearer "+f.token+strconv.Itoa(i))

			response, err := internal.Do(request)
			if err != nil {
				t.Fatal(err)
			}

			image, decodeErr := wire.DecodePublication(response.Body)

			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil || closeErr != nil {
				t.Fatalf("replication status=%d decode=%v close=%v", response.StatusCode, decodeErr, closeErr)
			}

			if err := r.a.Replication.installReplica(ctx, r.ctx, image); err != nil {
				t.Fatal(err)
			}

			_, err = r.a.authority.Current()
			if err != nil || capturePublication(t, r.a.authority).encoded != publication.encoded {
				t.Fatal("replica diverged", err)
			}
		}

		t.Logf("replication sequence=%d elapsed=%s mock_token_reviews=%d", publication.record.Sequence, time.Since(start), reviews.Load())
	}
	replicate(base)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+wire.SnapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := internal.Do(request)
	responseBody(t, response, err, http.StatusUnauthorized)

	request, err = http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+ReplicationPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err = internal.Do(request)
	responseBody(t, response, err, http.StatusUnauthorized)

	// Keep production freshness bounds; periodic authority observations are fake
	// Kubernetes reads, never fake dataplane polls or extended freshness windows.
	observed := make(chan struct{})

	go func() {
		defer close(observed)

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, r := range replicas {
					if r.ctx.Err() == nil {
						r.a.Replication.observe(r.ctx)
					}
				}
			}
		}
	}()

	defer func() { cancel(); <-observed }()

	t.Logf("clients=%d replicas=3 GOMAXPROCS=%d setup=%s full_bytes=%d max_writes=%d max_auth=%d", count, runtime.GOMAXPROCS(0), time.Since(setup), len(base.encoded), leader.a.Server.Config.Limits.MaxConcurrentWrites, leader.a.Server.Config.Limits.MaxConcurrentBootstrap)
	replicationSmokeStats(t, "baseline", replicas)

	var (
		overloaded, reconnected atomic.Int64
		workers                 sync.WaitGroup
	)

	defer func() { cancel(); workers.Wait() }()

	run := func(after wire.Sequence, cold bool) <-chan replicationSmokeResult {
		results := make(chan replicationSmokeResult, count)
		slots := make(chan struct{}, 24)

		for i := range peers {
			workers.Go(func() {
				if cold {
					select {
					case slots <- struct{}{}:
					case <-ctx.Done():
						results <- replicationSmokeResult{index: i, err: ctx.Err()}
						return
					}

					defer func() { <-slots }()
				}

				start := time.Now()
				result := replicationSmokeResult{index: i}

				defer func() { result.elapsed = time.Since(start); results <- result }()

				for attempt := range 6 {
					peer := &peers[i]
					r := replicas[peer.replica]

					path := r.endpoint + wire.SnapshotPath
					if after != 0 {
						path += fmt.Sprintf("?after=%d", after)
					}

					request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
					if err != nil {
						result.err = err
						return
					}

					response, err := peer.client.Do(request)
					if err != nil {
						if r.ctx.Err() != nil && ctx.Err() == nil {
							peer.replica = i % 2

							reconnected.Add(1)

							continue
						}

						result.err = err

						return
					}

					hash := sha256.New()
					result.bytes, err = io.Copy(hash, io.LimitReader(response.Body, wire.MaxPublicationBytes+1))

					closeErr := response.Body.Close()
					if r.ctx.Err() != nil && ctx.Err() == nil && (err != nil || response.StatusCode == http.StatusServiceUnavailable) {
						peer.replica = i % 2

						reconnected.Add(1)

						continue
					}

					if err != nil || closeErr != nil {
						result.err = fmt.Errorf("body read=%v close=%v", err, closeErr)
						return
					}

					if response.StatusCode == http.StatusTooManyRequests {
						overloaded.Add(1)

						if !replicationSleep(ctx, time.Second+time.Duration((i*31+attempt*97)%900)*time.Millisecond) {
							break
						}

						continue
					}

					if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
						result.err = fmt.Errorf("HTTP status %d or missing TLS 1.3", response.StatusCode)
						return
					}

					copy(result.digest[:], hash.Sum(nil))

					return
				}

				result.err = fmt.Errorf("six-attempt request budget exhausted")
			})
		}

		return results
	}

	collect := func(phase string, started time.Time, results <-chan replicationSmokeResult, want *CommittedPublication) {
		t.Helper()

		digest := sha256.Sum256([]byte(want.encoded))
		latencies := make([]time.Duration, 0, count)

		var total int64

		for range peers {
			select {
			case result := <-results:
				if result.err != nil || result.digest != digest || result.bytes != int64(len(want.encoded)) {
					t.Fatalf("%s client=%d bytes=%d err=%v digest_match=%v", phase, result.index, result.bytes, result.err, result.digest == digest)
				}

				total += result.bytes
				latencies = append(latencies, result.elapsed)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}

		slices.Sort(latencies)
		t.Logf("phase=%s clients=%d all_delivered=%s request_p50=%s request_p99=%s bytes=%d cumulative_429=%d reconnects=%d", phase, count, time.Since(started), latencies[len(latencies)/2], latencies[(len(latencies)-1)*99/100], total, overloaded.Load(), reconnected.Load())
		replicationSmokeStats(t, phase, replicas)
	}
	start := time.Now()
	collect("cold", start, run(0, true), base)

	for phase := range 2 {
		start = time.Now()
		results := run(base.record.Sequence, false)
		replicationSmokePark(t, ctx, replicas, count, results)
		replicationSmokeStats(t, "parked", replicas)

		if phase == 1 {
			replicas[2].cancel()
			replicationSmokePark(t, ctx, replicas[:2], count, results)
			t.Logf("failure_repark=%s", time.Since(start))
			replicationSmokeStats(t, "failure-parked", replicas)
		}

		for id, member := range accepted {
			member.Shares++
			accepted[id] = member
		}

		start = time.Now()
		base = replicationSmokePublish(t, ctx, f.a.Topology, accepted)

		image, err := wire.DecodePublication(strings.NewReader(base.encoded))
		if err != nil {
			t.Fatal(err)
		}

		if err := leader.a.Replication.installReplica(ctx, leader.ctx, image); err != nil {
			t.Fatal(err)
		}

		replicate(base)
		collect([]string{"update", "replica-failure-update"}[phase], start, results, base)
	}

	if reconnected.Load() != int64(count/3) {
		t.Fatalf("reconnected=%d want=%d", reconnected.Load(), count/3)
	}
}

func replicationSmokeLifecycle(l *Lifecycle, ctx context.Context) {
	l.process, l.synced = ctx, true
}

func replicationSmokePublish(t *testing.T, ctx context.Context, r *TopologyReconciler, accepted AcceptedMembers) *CommittedPublication {
	t.Helper()

	_, err := r.authority.PublishTopology(ctx, func(context.Context) (TopologyObservation, error) {
		nodes := corev1.NodeList{}

		for id, member := range accepted {
			encoded, err := json.Marshal(member)
			if err != nil {
				return TopologyObservation{}, err
			}

			nodes.Items = append(nodes.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: string(id), UID: types.UID(id), Annotations: map[string]string{admittedMemberAnnotation: string(encoded), wire.SharesAnnotation: strconv.FormatUint(uint64(member.Shares), 10)}}})
		}

		return TopologyObservation{Nodes: nodes, Input: members.Input{Nodes: nodes.Items, PeerPort: r.Config.PeerPort}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return capturePublication(t, r.authority)
}

func replicationSmokePark(t *testing.T, ctx context.Context, replicas []*replicationSmokeReplica, count int, results <-chan replicationSmokeResult) {
	t.Helper()

	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for {
		select {
		case result := <-results:
			t.Fatalf("client %d completed before publication: %v", result.index, result.err)
		default:
		}

		total := 0

		for _, r := range replicas {
			total += r.a.Server.polls.count()
		}

		if total == count {
			return
		}

		if !replicationSleep(deadline, 10*time.Millisecond) {
			t.Fatalf("parked %d/%d real HTTPS requests before deadline", total, count)
		}
	}
}

func replicationSmokeStats(t *testing.T, phase string, replicas []*replicationSmokeReplica) {
	t.Helper()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var polls, live, accepted []int64

	for _, r := range replicas {
		polls = append(polls, int64(r.a.Server.polls.count()))
		live = append(live, r.listener.live.Load())
		accepted = append(accepted, r.listener.accepted.Load())
	}

	status, _ := os.ReadFile("/proc/self/status")

	var resource []string

	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "Threads:") {
			resource = append(resource, strings.TrimSpace(line))
		}
	}

	fds, _ := os.ReadDir("/proc/self/fd")
	t.Logf("resources phase=%s polls=%v live_tcp=%v accepted_tcp=%v heap=%d stack=%d total_alloc=%d goroutines=%d fd=%d process=%v", phase, polls, live, accepted, mem.HeapAlloc, mem.StackInuse, mem.TotalAlloc, runtime.NumGoroutine(), len(fds), resource)
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
					synctest.Test(t, func(t *testing.T) {
						f := newServingFixture(t)
						s := f.a.Server

						request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)

						switch route {
						case "bootstrap":
							encoded, err := wire.EncodeBootstrapRequest(f.request)
							if err != nil {
								t.Fatal(err)
							}

							request = httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Authorization", "Bearer "+f.token)
						case "keyring":
							request = httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
						case "keyring empty":
							request = httptest.NewRequest(http.MethodGet, wire.KeyringPath+"?after=1", nil)
							// Keep both stores fresh throughout the no-change poll.
							configureFixtureAge(t, f, 2*wire.PollWait)
						}

						request.TLS = f.requestState(t)
						called := false
						changeAuthority := func() {
							called = true

							if change != "rotate" {
								withdrawServerTrust(t, s)
							}

							if change != "invalidate" {
								if change == "recover" {
									restoreServerTrust(t, s)
								} else {
									rotateFixtureTrust(t, f)
								}
							}
							// Deliberately do not yield or wait for cancellation callbacks.
						}

						w := &completionResponse{ResponseRecorder: httptest.NewRecorder()}
						if stage == "write" {
							w.onWrite = changeAuthority
						} else {
							w.onFlush = changeAuthority
						}

						var aborted any

						func() {
							defer func() { aborted = recover() }()

							s.Handler().ServeHTTP(w, request)
						}()

						if !called {
							t.Fatal("completion hook not reached", w.Code)
						}

						if change == "rotate" {
							if aborted != nil {
								t.Fatalf("ordinary rotation aborted admitted response: %v", aborted)
							}
						} else if aborted != http.ErrAbortHandler {
							t.Fatalf("revoked response completed: %v", aborted)
						}

						if len(s.writes) != 0 || len(s.bootstrapSlots) != 0 || s.keyringPolls.count() != 0 || s.polls.count() != 0 {
							t.Fatal("completion leaked admission")
						}
					})
				})
			}
		}
	}
}

func TestTrustAuthoritySynchronousRevocation(t *testing.T) {
	f := newServingFixture(t)

	ctx, cancel, err := f.a.authority.TrustContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	withdrawServerTrust(t, f.a.Server)

	if ctx.Err() != context.Canceled {
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

					request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
					if route == "keyring" {
						request = httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
					}

					if route == "bootstrap" {
						encoded, err := wire.EncodeBootstrapRequest(f.request)
						if err != nil {
							t.Fatal(err)
						}

						request = httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Authorization", "Bearer "+f.token)
					}

					request.TLS = f.requestState(t)
					request = request.WithContext(connectionContext(f.ctx, server))
					w := &pipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server}
					done := make(chan any, 1)

					go func() { defer func() { done <- recover() }(); s.Handler().ServeHTTP(w, request) }()

					synctest.Wait()

					if len(s.writes) != 1 {
						t.Fatal("response not blocked in write")
					}

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

						if _, err := f.a.authority.ReconcileCredentials(t.Context()); err != nil {
							t.Fatal(err)
						}

						reconcileTopology(t, f.a.Topology, f.ctx)

						time.Sleep(2 * time.Second)
					}

					if aborted := <-done; aborted != http.ErrAbortHandler {
						t.Fatalf("blocked response did not abort: %v", aborted)
					}

					want := time.Duration(0)
					if change == "expire" || change == "reconfirm" {
						want = 5 * time.Second
					}

					if elapsed := time.Since(start); elapsed != want {
						t.Fatalf("trust cancellation took %s, want %s", elapsed, want)
					}

					if f.a.authority.PublicationReady() != nil {
						t.Fatal("test must retain fresh publication independently of trust")
					}

					if len(s.writes) != 0 || len(s.bootstrapSlots) != 0 {
						t.Fatal("write or bootstrap admission leaked")
					}

					if change == "invalidate recover" || change == "reconfirm" {
						if err := f.a.authority.TrustReady(); err != nil {
							t.Fatal("new requests should have usable trust", err)
						}
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

		ctx, cancel, err := f.a.authority.TrustContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()

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

type cachedScaleClient struct {
	client.Client
	reader client.Reader
}

func (c cachedScaleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c cachedScaleClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

// The informer uses a synthetic list/watch HTTP source, but the cache, field
// index, deep copies, reconciler, canonical hashes, encoding and waiting are real.
// Only durable version CAS uses a fake client. This is deliberately not an HTTPS
// authentication or API-server capacity benchmark.
func TestServerScale(t *testing.T) {
	if os.Getenv("RACER_SCALE") != "1" {
		t.Skip("set RACER_SCALE=1 for 100,000-member reconciliation and waiter measurements")
	}

	for _, count := range []int{1_000, 10_000, 100_000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := initializedTopology(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reader := scaleCache(t, r, count)
			r.Client = cachedScaleClient{Client: r.Client, reader: reader}

			runtime.GC()

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			start := time.Now()
			first := reconcileTopology(t, r, ctx)
			cold := time.Since(start)

			runtime.ReadMemStats(&after)

			if len(acceptedMembers(t, r)) != count {
				t.Fatalf("accepted %d of %d", len(acceptedMembers(t, r)), count)
			}

			t.Logf("members=%d cold_reconcile=%s allocated_bytes=%d publication_bytes=%d", count, cold, after.TotalAlloc-before.TotalAlloc, len(first.encoded))

			start = time.Now()

			if current := reconcileTopology(t, r, ctx); current != first {
				t.Fatal("no-op reconcile replaced publication")
			}

			t.Logf("members=%d unchanged_reconcile=%s", count, time.Since(start))

			if count == 100_000 {
				scaleFanout(t, r, ctx, count)
			}
		})
	}
}

func scaleCache(t *testing.T, r *TopologyReconciler, count int) cache.Cache {
	t.Helper()

	nodes := &corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	pods := &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}

	for i := range count {
		uid := types.UID(fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))
		node := memberNode()
		node.Name, node.UID, node.ResourceVersion = fmt.Sprintf("node-%d", i), uid, "1"
		node.Annotations = map[string]string{wire.RDMANICsAnnotation: `[{"rail":0,"device":"mlx5_0","port":1,"numa_node":0},{"rail":1,"device":"mlx5_1","port":1,"numa_node":1}]`}
		pod := memberPod(uid, 1, fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255))
		pod.Spec.NodeName, pod.ResourceVersion = node.Name, "1"
		pod.OwnerReferences[0].Name = r.Config.DaemonSetName

		nodes.Items, pods.Items = append(nodes.Items, node), append(pods.Items, pod)
	}

	ds := &appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.DaemonSet{{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName, UID: testDaemonSetUID, ResourceVersion: "1"}}}}

	caches := &racerv1.ClusterCacheList{TypeMeta: metav1.TypeMeta{APIVersion: racerv1.GroupVersion.String(), Kind: "ClusterCacheList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	for i := range 16 {
		caches.Items = append(caches.Items, catalogCache(fmt.Sprintf("cache-%d", i), types.UID(fmt.Sprintf("%08x-1111-4000-8000-000000000000", i))))
		if err := r.Create(t.Context(), &caches.Items[i]); err != nil {
			t.Fatal(err)
		}
	}

	runKeys(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)

	lists := map[string]any{
		"/api/v1/nodes": nodes,
		"/api/v1/namespaces/" + r.Config.Namespace + "/pods":             pods,
		"/apis/apps/v1/namespaces/" + r.Config.Namespace + "/daemonsets": ds,
		"/apis/" + racerv1.GroupVersion.String() + "/clustercaches":      caches,
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if req.URL.Query().Get("sendInitialEvents") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonBadRequest, Code: 400, Message: "synthetic source supports ordinary list/watch"})

			return
		}

		if req.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			<-req.Context().Done()

			return
		}

		list, ok := lists[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}

		json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(source.Close)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, appsv1.SchemeGroupVersion, racerv1.GroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("PodList"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("DaemonSet"), meta.RESTScopeNamespace)
	mapper.Add(racerv1.GroupVersion.WithKind("ClusterCache"), meta.RESTScopeRoot)

	options := cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:       {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
		&corev1.Secret{}:    {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}, Field: fields.OneTermEqualSelector("metadata.name", r.Config.CredentialsSecretName)},
		&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
		&appsv1.DaemonSet{}: {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
	}}
	options.Scheme, options.Mapper = r.Scheme(), mapper

	reader, err := cache.New(&rest.Config{Host: source.URL, QPS: 1000, Burst: 1000}, options)
	if err != nil {
		t.Fatal(err)
	}

	if err := reader.IndexField(t.Context(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		t.Fatal(err)
	}

	for _, obj := range []client.Object{&corev1.Node{}, &appsv1.DaemonSet{}, &racerv1.ClusterCache{}} {
		if _, err := reader.GetInformer(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- reader.Start(ctx) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	syncCtx, stopSync := context.WithTimeout(ctx, time.Minute)
	defer stopSync()

	if !reader.WaitForCacheSync(syncCtx) {
		t.Fatal("scale informer did not synchronize")
	}

	return reader
}

func scaleFanout(t *testing.T, r *TopologyReconciler, ctx context.Context, count int) {
	t.Helper()

	current, err := r.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	// Keep both realistic full-size encodings alive. Prepare before admission so
	// fanout measures Install plus delivery, independent of canonical encoding.
	members := make(AcceptedMembers, count)

	for id, member := range acceptedMembers(t, r) {
		member.Shares++
		members[id] = member
	}

	sequence := current.Sequence()
	server := &Server{Config: r.Config.ServerConfig}
	server.initializeAdmission()

	waiting, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan *authority.PublicationHandle, count)
	failures := make(chan error, count)

	var wg sync.WaitGroup

	runtime.GC()
	runtime.GC() // Clear temporary encoding sync.Pools before the waiter baseline.

	var before, parked runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()

	for id := range members {
		wg.Go(func() {
			if !server.admitPoll(id) {
				results <- nil

				failures <- wire.Overloaded

				return
			}
			defer server.releasePoll(id)

			p, err := waitFixturePublication(waiting, r.authority, sequence)
			results <- p

			failures <- err
		})
	}

	defer wg.Wait()
	defer cancel()

	eventually(t, "100000 admitted waiters", func() bool {
		return server.polls.count() == count
	})

	admit := time.Since(start)

	runtime.GC()
	runtime.ReadMemStats(&parked)

	if server.admitPoll(testOtherUID) {
		t.Fatal("global bound failed")
	}

	for id := range members {
		if server.admitPoll(id) {
			t.Fatal("duplicate bound failed")
		}

		break
	}

	start = time.Now()

	next := replicationSmokePublish(t, ctx, r, members)

	install := time.Since(start)

	wg.Wait()

	fanout := time.Since(start)

	for range count {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}

		if got := <-results; got == nil || got.Sequence() != next.record.Sequence {
			t.Fatal("waiter missed or copied full publication")
		}
	}

	awaitServerPolls(t, server, 0)
	t.Logf("waiters=%d GOMAXPROCS=%d admission=%s install=%s all_delivered=%s heap_delta=%d stack_delta=%d next_bytes=%d", count, runtime.GOMAXPROCS(0), admit, install, fanout, int64(parked.HeapAlloc)-int64(before.HeapAlloc), int64(parked.StackInuse)-int64(before.StackInuse), len(next.encoded))
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
		if err := r.Client.Create(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	username := "system:serviceaccount:" + r.Config.Namespace + ":racer-controller"
	audience := ReplicationAudience
	r.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review := obj.(*authv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != ReplicationAudience {
			t.Fatal("wrong review audience")
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience}, User: authv1.UserInfo{Username: username, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})
	fixtureDependencies[r.authority].Client = r.Client

	for _, unchanged := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocked flush unchanged=%v failure=%v", unchanged, fail), func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, ReplicationPath, nil)
				request.TLS = f.requestState(t)
				request.Header.Set("Authorization", "Bearer "+f.token)

				want := http.StatusOK

				if unchanged {
					p, err := r.authority.Current()
					if err != nil {
						t.Fatal(err)
					}

					request.URL.RawQuery = fmt.Sprintf("after=%d", p.Sequence())
					want = http.StatusNoContent
				}

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true, fail: fail}

				unblock := sync.OnceFunc(func() { close(w.unblock) })
				defer unblock()

				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				select {
				case <-w.entered:
				case <-time.After(8 * time.Second):
					t.Fatal("explicit flush not reached")
				}

				if len(f.a.Server.writes) != 1 {
					t.Fatal("write admission released before flush")
				}

				held := f.a.Server.replicationPolls.count() == 1

				if !held {
					t.Fatal("poll admission released before flush")
				}

				duplicate := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(duplicate, request.Clone(f.ctx))

				if duplicate.Code != http.StatusTooManyRequests {
					t.Fatalf("duplicate during flush: %d", duplicate.Code)
				}

				unblock()

				var wantAbort any
				if fail {
					wantAbort = http.ErrAbortHandler
				}

				if aborted := <-done; aborted != wantAbort || w.Code != want {
					t.Fatalf("flush result %v status %d", aborted, w.Code)
				}

				if len(f.a.Server.writes) != 0 {
					t.Fatal("write admission leaked after flush")
				}

				held = f.a.Server.replicationPolls.count() != 0

				if held {
					t.Fatal("poll admission leaked after flush")
				}
			})
		}
	}

	for _, tc := range []struct {
		name string
		code int
	}{{"controller", 200}, {"duplicate poll", 429}, {"dataplane", 403}, {"wrong audience", 401}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "duplicate poll" {
				f.a.Server.initializeAdmission()

				if !f.a.Server.replicationPolls.acquire(string(pod.UID)) {
					t.Fatal("could not reserve replication poll")
				}
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

			if response.Code != tc.code {
				t.Fatal(response.Code, response.Body.String())
			}
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
	p := reconcileTopology(t, r, t.Context())
	copy := *p
	copy.delta = "delta"
	copy.deltaBase = "base"
	delta := copy.ForBase("base")

	writeCtx, cancel, err := copy.writeContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	restore := withdrawPublication(t, r)
	restore()
	reconcileTopology(t, r, t.Context())

	<-writeCtx.Done()

	if _, err := delta.writeTo(writeCtx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("revoked delta: %v", err)
	}

	if _, _, err := copy.writeContext(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("old image borrowed new authority: %v", err)
	}
}

func TestSnapshotAuthorityHeldThroughFlush(t *testing.T) {
	for _, action := range []string{"suspend", "freshness"} {
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
				if err != nil {
					t.Fatal(err)
				}

				if action == "suspend" {
					restore := withdrawPublication(t, f.a.Topology)
					restore()
					reconcileTopology(t, f.a.Topology, f.ctx)
				} else {
					time.Sleep(3 * time.Second)

					if err := f.a.authority.Observe(f.ctx); err != nil {
						t.Fatal(err)
					}

					time.Sleep(2 * time.Second)
				}

				if len(f.a.Server.writes) != 1 || f.a.Server.polls.count() != 1 {
					t.Fatal("flush released admission")
				}

				close(w.unblock)

				if aborted := <-done; aborted != http.ErrAbortHandler {
					t.Fatalf("revoked flush completed: %v", aborted)
				}

				if len(f.a.Server.writes) != 0 || f.a.Server.polls.count() != 0 {
					t.Fatal("aborted flush leaked admission")
				}
			})
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
			if err := a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
				t.Fatal(err)
			}

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
			case "recreated pod":
				pod.UID = "replacement"
			case "recreated sa", "recreated ds":
				var obj client.Object = &corev1.ServiceAccount{}
				if scenario == "recreated ds" {
					obj = &appsv1.DaemonSet{}
				}

				if err := a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}, obj); err != nil {
					t.Fatal(err)
				}

				obj.SetUID("replacement")

				if err := a.Topology.Update(ctx, obj); err != nil {
					t.Fatal(err)
				}
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
			case "terminal pod":
				pod.Status.Phase = corev1.PodFailed
				if err := a.Topology.Client.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "excluded node", "deleted node":
				node := &corev1.Node{}
				if err := a.Topology.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
					t.Fatal(err)
				}

				if scenario == "deleted node" {
					if err := a.Topology.Delete(ctx, node); err != nil {
						t.Fatal(err)
					}
				} else {
					node.Labels = map[string]string{wire.ExclusionLabel: ""}
					if err := a.Topology.Update(ctx, node); err != nil {
						t.Fatal(err)
					}
				}
			case "expired token":
				token = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			}

			if err := a.Topology.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}

			installReview(t, a, status, token)

			if scenario == "api failure" {
				fixtureDependencies[a.authority].reader = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return fmt.Errorf("private upstream failure")
				}})
			}

			if scenario == "canceled" {
				cancel()
			}

			body, err := wire.EncodeBootstrapRequest(enrollment)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequestWithContext(ctx, http.MethodPost, wire.BootstrapPath, bytes.NewReader(body))
			req.TLS = &tls.ConnectionState{HandshakeComplete: true}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)

			if scenario == "duplicate bearer" {
				req.Header.Add("Authorization", "Bearer "+token)
			}

			w := httptest.NewRecorder()
			a.Server.Handler().ServeHTTP(w, req)

			if scenario == "success" {
				issued := decodeIssuedResponse(t, responseBody(t, w.Result(), nil, http.StatusOK))

				leaf, err := x509.ParseCertificate(issued.CertificateChain[0])
				if err != nil || issued.Node != wire.NodeID(testNodeUID) || issued.Cluster != a.Topology.Config.Cluster || !leaf.NotAfter.After(time.Now()) {
					t.Fatalf("issued identity: %+v %v", issued, err)
				}
			} else {
				want := http.StatusForbidden

				switch scenario {
				case "audience", "not authenticated", "review error", "missing bound pod", "ambiguous bound pod", "expired token", "duplicate bearer":
					want = http.StatusUnauthorized
				case "api failure", "canceled":
					want = http.StatusServiceUnavailable
				}

				responseBody(t, w.Result(), nil, want)

				var node corev1.Node
				if scenario != "deleted node" {
					if err := a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node); err != nil {
						t.Fatal(err)
					}

					if node.Annotations[enrolledSharesAnnotation] != "" || node.Annotations[enrolledRDMANICsAnnotation] != "" {
						t.Fatal("rejected enrollment persisted hardware proposal")
					}
				}
			}
		})
	}
}

func TestEnrollmentSaturationPreservesLocalAuthentication(t *testing.T) {
	f := newServingFixture(t)
	s := f.a.Server
	s.Config.Limits.MaxConcurrentBootstrap = 1
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
			s := f.a.Server
			s.Config.Limits.MaxConcurrentWrites = 1
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

			body, err := wire.EncodeBootstrapRequest(f.request)
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequestWithContext(ctx, "POST", wire.BootstrapPath, bytes.NewReader(body))
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header.Set("Authorization", "Bearer "+f.token)
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			encoded := responseBody(t, w.Result(), nil, wantStatus)
			if reads == 0 || len(s.bootstrapSlots) != 0 || len(s.authSlots) != 0 || len(s.writes) != wantWrites {
				t.Fatal("issuance skipped or admission leaked")
			}

			if scenario == "success" {
				response := decodeIssuedResponse(t, encoded)
				if response.Enrollment != f.request.Enrollment || response.Node != wire.NodeID(testNodeUID) || !w.Flushed || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("issued response was not served completely")
				}
			}
		})
	}
}
