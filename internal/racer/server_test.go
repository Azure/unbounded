// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type servingFixture struct {
	a                 *Application
	token             string
	key               ed25519.PrivateKey
	request           wire.BootstrapRequest
	certificate       tls.Certificate
	serverCertificate tls.Certificate
	roots             *x509.CertPool
	ctx               context.Context
	cancel            context.CancelFunc
}

func newServingFixture(t *testing.T) *servingFixture {
	t.Helper()
	a, status, token := authFixture(t)
	installReview(t, a, status, token)
	// Freeze the hint so live revocation tests also exercise informer lag.
	var node corev1.Node
	if err := a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node); err != nil {
		t.Fatal(err)
	}

	a.Server.NodeHints = fake.NewClientBuilder().WithScheme(a.Topology.Scheme()).WithObjects(&node).WithIndex(&corev1.Node{}, nodeUIDIndex, nodeUIDKeys).Build()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, ctx)
	a.Lifecycle.mu.Lock()
	a.Lifecycle.leader, a.Lifecycle.synced, a.Lifecycle.serving = ctx, true, true
	a.Lifecycle.mu.Unlock()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"attacker"}, Subject: pkix.Name{CommonName: "attacker"}}, key)
	if err != nil {
		t.Fatal(err)
	}

	request := wire.BootstrapRequest{SchemaVersion: 1, Cluster: a.Server.Config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr}
	identity := NodeIdentity{cluster: request.Cluster, node: wire.NodeID(testNodeUID), expires: time.Now().Add(time.Hour)}

	response, err := a.Keyring.Issuer.Issue(ctx, identity, request)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert)

	return &servingFixture{a: a, token: token, key: key, request: request, certificate: tls.Certificate{Certificate: response.CertificateChain, PrivateKey: key}, serverCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots, ctx: ctx, cancel: cancel}
}

func (f *servingFixture) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()

	config := &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	if cert != nil {
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}

	transport := &http.Transport{TLSClientConfig: config}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func (f *servingFixture) start(t *testing.T) string {
	t.Helper()

	s := httptest.NewUnstartedServer(f.a.Server.Handler())
	s.Config.ConnContext = connectionContext
	s.TLS = f.a.Server.tlsConfig(f.ctx, f.serverCertificate)
	s.StartTLS()
	t.Cleanup(s.Close)

	return s.URL
}

func responseBody(t *testing.T, response *http.Response, err error, status int) []byte {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()

	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != status {
		t.Fatalf("status %d, want %d: %s", response.StatusCode, status, b)
	}

	if status >= 400 {
		if _, err := wire.DecodeError(bytes.NewReader(b)); err != nil {
			t.Fatalf("non-protocol error %q", b)
		}

		if status == 429 || status == 503 {
			if response.Header.Get("Retry-After") != "1" {
				t.Fatal("missing retry bound")
			}
		}
	}

	return b
}

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
		{"GET", "/v1/snapshot?after=999", 409},
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
	for name, mutate := range map[string]func(*x509.Certificate){
		"expired":          func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Second) },
		"future":           func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) },
		"wrong usage":      func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"wrong cluster":    func(c *x509.Certificate) { c.URIs[0].Host = testNodeUID },
		"wrong uid":        func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID },
		"ambiguous SAN":    func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) },
		"query SAN":        func(c *x509.Certificate) { c.URIs[0].RawQuery = "admin=true" },
		"no signing usage": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment },
	} {
		t.Run(name, func(t *testing.T) {
			cert := f.signLeaf(t, mutate)

			response, err := f.client(t, &cert).Get(endpoint + wire.SnapshotPath)
			if err == nil {
				defer response.Body.Close()

				if response.StatusCode != 401 && response.StatusCode != 403 && (name != "wrong uid" || response.StatusCode != 503) {
					t.Fatalf("bad identity admitted: %d", response.StatusCode)
				}
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
	responseBody(t, response, err, 200)
}

func TestPooledTLSRechecksLiveAuthorizationAndExpiry(t *testing.T) {
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

			want := 403
			if scenario == "expired" {
				want = 401
			}

			responseBody(t, response, err, want)

			if !reused {
				t.Fatal("test failed to reuse TLS connection")
			}
		})
	}
}

func TestHTTPSFirstSnapshotRetriesMissingNodeHint(t *testing.T) {
	f := newServingFixture(t)
	hints := fake.NewClientBuilder().WithScheme(f.a.Topology.Scheme()).WithIndex(&corev1.Node{}, nodeUIDIndex, nodeUIDKeys).Build()
	f.a.Server.NodeHints = hints
	endpoint := f.start(t)
	c := f.client(t, &f.certificate)
	response, err := c.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, 503)

	var node corev1.Node
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node); err != nil {
		t.Fatal(err)
	}
	// Simulate the informer catching up without replacing the persisted identity.
	node.ResourceVersion = ""
	if err := hints.Create(f.ctx, &node); err != nil {
		t.Fatal(err)
	}

	response, err = c.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, 200)
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
	material.Keys[id] = signingMaterial{Certificate: der, PrivateKey: key}

	issuer := &corev1.Secret{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Namespace: "racer", Name: f.a.Keyring.Config.IssuerSecretName}, issuer); err != nil {
		t.Fatal(err)
	}

	issuer.Data["issuer.json"], err = json.Marshal(material)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.a.Topology.Update(f.ctx, issuer); err != nil {
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
	responseChain, err := f.a.Keyring.Issuer.Issue(f.ctx, NodeIdentity{cluster: f.request.Cluster, node: wire.NodeID(testNodeUID), expires: time.Now().Add(time.Hour)}, f.request)
	if err != nil {
		t.Fatal(err)
	}

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
}

func (w *blockingResponse) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.unblock })
	return w.ResponseRecorder.Write(b)
}
func (w *blockingResponse) Unwrap() http.ResponseWriter { return w.ResponseRecorder }

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
	f := newServingFixture(t)
	handler := f.a.Server.Handler()
	r := httptest.NewRequest("GET", wire.SnapshotPath, nil)
	r.TLS = f.requestState(t)
	w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
	done := make(chan struct{})

	go func() { defer close(done); handler.ServeHTTP(w, r) }()

	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, r.Clone(f.ctx))

	if second.Code != 429 {
		t.Fatalf("overlapping node write admitted: %d", second.Code)
	}

	close(w.unblock)
	<-done

	third := httptest.NewRecorder()
	handler.ServeHTTP(third, r.Clone(f.ctx))

	if third.Code != 200 {
		t.Fatalf("admission leaked: %d", third.Code)
	}
}

func TestAdmissionAndAPIDeadlines(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxConcurrentBootstrap = 1
	f.a.Server.Config.Limits.WriteTimeout = 100 * time.Millisecond
	entered := make(chan struct{}, 1)
	f.a.Server.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
		entered <- struct{}{}

		<-ctx.Done()

		return ctx.Err()
	}})
	handler := f.a.Server.Handler()
	r := httptest.NewRequest("GET", wire.SnapshotPath, nil)
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

func TestOperationalStartCancellationAndTLSFiles(t *testing.T) {
	f := newServingFixture(t)
	dir := t.TempDir()
	f.a.Server.Config.ControlAddress = "127.0.0.1:0"
	f.a.Server.Config.TLSCertificateFile = filepath.Join(dir, "tls.crt")

	f.a.Server.Config.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(f.a.Server.Config.TLSCertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.serverCertificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := x509.MarshalPKCS8PrivateKey(f.key)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(f.a.Server.Config.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}

	f.a.Lifecycle.SetServingReady(false)

	done := make(chan error, 1)

	go func() { done <- f.a.Server.Start(f.ctx) }()

	deadline := time.After(5 * time.Second)

	for f.a.Server.Ready(nil) != nil {
		select {
		case err := <-done:
			t.Fatalf("start: %v", err)
		case <-deadline:
			t.Fatal("not ready")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	f.cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}

	if f.a.Server.Ready(nil) == nil {
		t.Fatal("canceled server ready")
	}
}

func TestLeaderCancellationClosesActiveTLSPoll(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.initializeAdmission()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- f.a.Server.serve(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate)) }()

	c := f.client(t, &f.certificate)

	publication, err := f.a.Server.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)

	go func() {
		response, err := c.Get(fmt.Sprintf("https://%s/v1/snapshot?after=%d", listener.Addr(), publication.record.Sequence))
		if response != nil {
			response.Body.Close()
		}

		requestDone <- err
	}()

	deadline := time.After(5 * time.Second)

	for {
		f.a.Server.admission.Lock()
		n := len(f.a.Server.polls)
		f.a.Server.admission.Unlock()

		if n == 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("poll not admitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	f.cancel()

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("leader cancellation left active poll")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server shutdown blocked")
	}
}

func TestTLSPollExpirationAndRequestCancellation(t *testing.T) {
	for _, expiration := range []bool{true, false} {
		t.Run(fmt.Sprint(expiration), func(t *testing.T) {
			f := newServingFixture(t)

			cert := f.certificate
			if expiration {
				cert = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(2 * time.Second).Truncate(time.Second) })
			}

			endpoint := f.start(t)
			c := f.client(t, &cert)

			publication, err := f.a.Server.Publications.Current()
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			r, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.record.Sequence), nil)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})

			go func() {
				defer close(done)

				response, err := c.Do(r)
				if expiration {
					responseBody(t, response, err, 401)
				} else {
					if response != nil {
						response.Body.Close()
					}

					if err == nil {
						t.Error("canceled poll succeeded")
					}
				}
			}()

			deadline := time.After(5 * time.Second)

			for {
				f.a.Server.admission.Lock()
				n := len(f.a.Server.polls)
				f.a.Server.admission.Unlock()

				if n == 1 {
					break
				}

				select {
				case <-deadline:
					t.Fatal("poll not admitted")
				default:
					time.Sleep(time.Millisecond)
				}
			}

			if !expiration {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("poll outlived expiration/cancellation")
			}

			for {
				f.a.Server.admission.Lock()
				n := len(f.a.Server.polls)
				f.a.Server.admission.Unlock()

				if n == 0 {
					break
				}

				select {
				case <-deadline:
					t.Fatal("poll admission leaked")
				default:
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

func TestHTTPWriteBootstrapAndGlobalAdmission(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxPolls = 1
	f.a.Server.Config.Limits.MaxConcurrentWrites = 1
	f.a.Server.Config.Limits.MaxConcurrentBootstrap = 1
	handler := f.a.Server.Handler()
	r := httptest.NewRequest("GET", wire.SnapshotPath, nil)

	r.TLS = f.requestState(t)
	for _, resource := range []string{"write", "global poll", "bootstrap", "headers"} {
		t.Run(resource, func(t *testing.T) {
			request := r.Clone(f.ctx)

			switch resource {
			case "write":
				take(f.a.Server.writes)
				defer release(f.a.Server.writes)
			case "global poll":
				f.a.Server.polls[wire.NodeID(testOtherUID)] = struct{}{}
				defer delete(f.a.Server.polls, wire.NodeID(testOtherUID))
			case "bootstrap":
				take(f.a.Server.bootstrapSlots)
				defer release(f.a.Server.bootstrapSlots)

				request.Method = "POST"
				request.URL.Path = wire.BootstrapPath
			case "headers":
				request.Header.Set("X-Large", strings.Repeat("a", f.a.Server.Config.Limits.HeaderBytes))
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			want := 429
			if resource == "headers" {
				want = 413
			}

			if w.Code != want {
				t.Fatalf("unbounded %s: %d", resource, w.Code)
			}
		})
	}
}

func TestBootstrapReadDeadlineAndChunkedBound(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.WriteTimeout = 100 * time.Millisecond
	endpoint := f.start(t)
	c := f.client(t, nil)
	// A body of unknown length must still be bounded by the wire decoder.
	r, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, io.NopCloser(strings.NewReader(strings.Repeat("x", wire.MaxBootstrapBytes+1))))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	response, err := c.Do(r)
	responseBody(t, response, err, 413)
	// A client that never completes its body cannot hold bootstrap admission.
	address := strings.TrimPrefix(endpoint, "https://")

	conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "POST /v1/bootstrap HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, conn)
	deadline := time.After(time.Second)

	for len(f.a.Server.bootstrapSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("slow body retained admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTLSSlowSnapshotWriteDeadline(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.WriteTimeout = 200 * time.Millisecond
	// Exercise socket backpressure without constructing a large topology. The
	// immutable publication remains valid JSON with bounded trailing whitespace.
	f.a.Server.Publications.mu.Lock()
	large := *f.a.Server.Publications.current
	large.encoded += strings.Repeat(" ", 16*1024*1024)
	f.a.Server.Publications.current = &large
	f.a.Server.Publications.mu.Unlock()
	endpoint := f.start(t)

	conn, err := tls.Dial("tcp", strings.TrimPrefix(endpoint, "https://"), &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)

	for len(f.a.Server.writes) == 0 {
		select {
		case <-deadline:
			t.Fatal("write not admitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Do not read response bytes. The write deadline must release both slots.
	for {
		f.a.Server.admission.Lock()
		n := len(f.a.Server.polls)
		f.a.Server.admission.Unlock()

		if n == 0 && len(f.a.Server.writes) == 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("slow socket bypassed write deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTLSRevocationWhilePolling(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	c := f.client(t, &f.certificate)

	publication, err := f.a.Server.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		response, err := c.Get(fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.record.Sequence))
		responseBody(t, response, err, 403)
	}()

	deadline := time.After(5 * time.Second)

	for {
		f.a.Server.Publications.mu.Lock()
		n := len(f.a.Server.Publications.polls)
		f.a.Server.Publications.mu.Unlock()

		if n == 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("poll not waiting")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	reconcileTopology(t, f.a.Topology, f.ctx)

	select {
	case <-done:
	case <-deadline:
		t.Fatal("revoked poll did not wake")
	}
}
