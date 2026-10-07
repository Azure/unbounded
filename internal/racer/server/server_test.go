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
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

var fixtureConfigs = map[*authority.Authority]fixtureConfig{}

type (
	NodeIdentity        = authority.NodeIdentity
	TopologyObservation = authority.TopologyObservation
	AcceptedMembers     = members.History
)

const (
	podNodeIndex               = "spec.nodeName"
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
	ReplicationAudience        = authority.ReplicationAudience
)

func podNodeKeys(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
}

func replicationSleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func catalogVolume(name string, uid types.UID) racerv1.ClusterVolume {
	return racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
}

func TestIdentityAdmission(t *testing.T) {
	a := newIdentityAdmission[string](1)
	b := newIdentityAdmission[string](1)

	require.True(t, a.acquire("one"))
	require.False(t, a.acquire("one"))
	require.False(t, a.acquire("two"))
	require.True(t, b.acquire("one"), "independent endpoint admission")
	a.release("one")
	require.True(t, a.acquire("two"))
	require.False(t, newIdentityAdmission[string](0).acquire("one"))
	require.False(t, newIdentityAdmission[string](-1).acquire("one"))
}

func TestAdmissionLimitsFrozen(t *testing.T) {
	cfg := testConfig(t).ServerConfig
	cfg.Limits.MaxPolls = 1
	cfg.Limits.MaxConcurrentBootstrap = 1
	s := New(cfg, nil, nil, nil, nil)
	cfg.Limits.MaxPolls = 2
	cfg.Limits.MaxConcurrentBootstrap = 2
	cfg.Limits.HeaderBytes = 1

	require.Equal(t, 1, s.polls.limit)
	require.Equal(t, 1, s.keyringPolls.limit)
	require.Equal(t, cap(s.bootstrapSlots), s.replicationPolls.limit)
	require.NotEqual(t, cfg.Limits.HeaderBytes, s.config.Limits.HeaderBytes)
}

const (
	testNodeUID                = "11111111-1111-4111-8111-111111111111"
	testOtherUID               = "22222222-2222-4222-8222-222222222222"
	testDaemonSetUID types.UID = "33333333-3333-4333-8333-333333333333"
)

func memberNode() corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testNodeUID}}
}

func memberPod(uid types.UID, created int64, ip string) corev1.Pod {
	controller := true

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "racer-" + string(uid), UID: uid, Namespace: "racer",
			CreationTimestamp: metav1.NewTime(time.Unix(created, 0)),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DataplaneDaemonSetName, UID: testDaemonSetUID, Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

const DataplaneDaemonSetName = members.DataplaneDaemonSetName

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
	require.Eventually(t, func() bool { return s.polls.count() == count }, 5*time.Second, time.Millisecond, "poll admission did not reach %d", count)
}

func bootstrapTestRequest(t *testing.T, ctx context.Context, endpoint, token string, request wire.BootstrapRequest) *http.Request {
	t.Helper()

	body, err := wire.EncodeBootstrapRequest(request)
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+wire.BootstrapPath, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	return r
}

func (f *servingFixture) publicRequest(t *testing.T, route string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
	if route == "keyring" {
		r.URL.Path = wire.KeyringPath
	}

	if route == "bootstrap" {
		r = bootstrapTestRequest(t, f.ctx, "", f.token, f.request)
	}

	if route == "keyring empty" {
		r = httptest.NewRequest(http.MethodGet, wire.KeyringPath+"?after=1", nil)

		configureFixtureAge(t, f, 2*wire.PollWait)
	}

	r.TLS = f.requestState(t)

	return r
}

func requireNoAdmission(t *testing.T, s *Server) {
	t.Helper()
	require.Empty(t, s.writes, "write admission leaked")
	require.Empty(t, s.bootstrapSlots, "bootstrap admission leaked")
	require.Zero(t, s.keyringPolls.count(), "keyring admission leaked")
	require.Zero(t, s.polls.count(), "snapshot admission leaked")
}

func serveRecover(handler http.Handler, w http.ResponseWriter, r *http.Request) (aborted any) {
	defer func() { aborted = recover() }()

	handler.ServeHTTP(w, r)

	return nil
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
				f.configureServer(func(c *Config) { c.Limits.MaxPolls = 1 })
				configureFixtureAge(t, f, 2*wire.PollWait)
				f.configureServer(func(c *Config) { c.Limits.MaxConcurrentWrites = 1 })
				other := *f
				other.certificate = f.signLeaf(t, func(c *x509.Certificate) { c.URIs[0].Path = "/node/" + testOtherUID })
				otherState := other.requestState(t)
				handler := f.a.Server.Handler()
				r := httptest.NewRequest("GET", wire.SnapshotPath, nil)

				r.TLS = f.requestState(t)
				r.URL.RawQuery = snapshotQueryForStatus(t, f, tc.status)

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

				require.Len(t, f.a.Server.writes, wantWrites, "write slots during response")

				unblock()

				aborted := <-done
				if tc.fail {
					require.Equal(t, http.ErrAbortHandler, aborted)
				} else {
					require.Nil(t, aborted)
					require.Equal(t, tc.status, w.Code)
				}

				require.Empty(t, f.a.Server.writes, "write slot leaked")

				third := httptest.NewRecorder()
				handler.ServeHTTP(third, r.Clone(f.ctx))

				require.Equal(t, http.StatusOK, third.Code, "admission leaked")
			})
		})
	}
}

func snapshotQueryForStatus(t *testing.T, f *servingFixture, status int) string {
	t.Helper()

	if status == http.StatusOK {
		return ""
	}

	current, err := f.a.authority.Current()
	require.NoError(t, err)

	cursor := current.Sequence()
	if status == http.StatusServiceUnavailable {
		cursor++
	}

	return fmt.Sprintf("after=%d", cursor)
}

func TestHTTPPollAdmissionAndCancellation(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := newServingFixture(t)
			f.configureServer(func(c *Config) { c.Limits.MaxPolls = limit })
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
	f.configureServer(func(c *Config) { c.Limits.MaxConcurrentBootstrap = 1 })

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
	f.configureServer(func(c *Config) {
		c.Limits.MaxConcurrentBootstrap = 1
		c.Limits.WriteTimeout = 100 * time.Millisecond
	})

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

func testConfig(t *testing.T) fixtureConfig {
	t.Helper()

	return fixtureConfig{
		Config: authority.Config{
			Cluster: testOtherUID, Namespace: "racer", DataplaneServiceAccount: "racer-dataplane",
			ControllerServiceAccount: "racer-controller", DaemonSetName: "racer-dataplane",
			CredentialsSecretName: "racer-credentials", VersionConfigMapName: "racer-version",
			InstallationConfigMapName: "racer-installation", CertificateLifetime: wire.CertificateLifetime,
			SnapshotMaxAge: 30 * time.Second, MaxTokenBytes: 16 * 1024,
			Rotation: authority.RotationPolicy{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour},
		},
		PeerPort: 8082, ServerConfig: Config{
			ControlAddress: ":8443", TLSCertificateFile: "/etc/racer/tls/tls.crt", TLSPrivateKeyFile: "/etc/racer/tls/tls.key", ReplicationServerName: "racer-controller.racer.svc",
			Limits: Limits{MaxConnections: 2*wire.MaxMembers + 128, MaxConcurrentHandshakes: 32, MaxPolls: wire.MaxMembers, MaxConcurrentWrites: 128, MaxConcurrentBootstrap: 32, HeaderBytes: 16 * 1024, HandshakeTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second, ShutdownTimeout: 10 * time.Second},
		},
	}
}

func testTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()
	cfg := testConfig(t)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, racerv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation-uid"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", "initialization_protocol": "staged-v1"}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithIndex(&corev1.Pod{}, podNodeIndex, podNodeKeys).Build()
	c = interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID(fmt.Sprintf("fake-%s-%d", obj.GetName(), time.Now().UnixNano())))
		}

		return c.Create(ctx, obj, opts...)
	}})

	return Assemble(cfg, c, c).Topology
}

func initializedTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()

	r := testTopology(t, objects...)
	if err := r.authority.Recover(t.Context(), r.Client); err != nil {
		t.Fatal(err)
	}

	return r
}

// Captured publication data is decoded through the public response operation,
// never an installation proof. It belongs exclusively to the integration test.
type (
	CommittedPublication struct {
		handle     *authority.PublicationHandle
		encoded    string
		record     VersionRecord
		leadership context.Context
	}
	VersionRecord struct {
		Cluster           wire.ClusterID
		Sequence          wire.Sequence
		MembershipVersion wire.MembershipVersion
		ContentHash       string
		MembershipHash    string
	}
)

func capturePublication(t *testing.T, a *authority.Authority) *CommittedPublication {
	t.Helper()

	h, err := a.Current()
	if err != nil {
		t.Fatal(err)
	}

	return captureHandle(t, h)
}

func captureHandle(t *testing.T, h *authority.PublicationHandle) *CommittedPublication {
	t.Helper()

	var b bytes.Buffer

	guard, cancel, err := h.Admit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	ctx := guard.Context()
	if _, err := h.ForBase(0, "").WriteTo(ctx, guard, &b); err != nil {
		t.Fatal(err)
	}

	p, err := wire.DecodePublication(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	content, members, err := wire.ContentHashes(p)
	if err != nil {
		t.Fatal(err)
	}

	return &CommittedPublication{handle: h, encoded: b.String(), record: VersionRecord{Cluster: p.Cluster, Sequence: p.Sequence, MembershipVersion: p.MembershipVersion, ContentHash: content, MembershipHash: members}, leadership: ctx}
}

func (p *CommittedPublication) admit(ctx context.Context) (*authority.Admission, context.CancelFunc, error) {
	return p.handle.Admit(ctx)
}

func reconcileTopology(t *testing.T, r *TopologyReconciler, ctx context.Context) *CommittedPublication {
	t.Helper()

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return capturePublication(t, r.authority)
}

func acceptedMembers(t *testing.T, r *TopologyReconciler) AcceptedMembers {
	t.Helper()

	p, err := r.authority.Current()
	if err != nil {
		return nil
	}

	captured := captureHandle(t, p)

	image, err := wire.DecodePublication(bytes.NewBufferString(captured.encoded))
	if err != nil {
		t.Fatal(err)
	}

	members := make(AcceptedMembers, len(image.Members))
	for _, m := range image.Members {
		members[m.Node] = m
	}

	return members
}

func runKeys(t *testing.T, r *KeyringReconciler) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return result
}

type (
	RotationState struct {
		NextRotation   time.Time            `json:"next_rotation"`
		ActivateAt     time.Time            `json:"activate_at"`
		ActiveIssuer   string               `json:"active_issuer"`
		PreparedIssuer string               `json:"prepared_issuer"`
		Retiring       map[string]time.Time `json:"retiring"`
	}
	signingMaterial struct {
		PrivateKey  []byte `json:"private_key"`
		Certificate []byte `json:"certificate"`
	}
	issuerMaterial struct {
		Keys map[string]signingMaterial `json:"keys"`
	}
)

func keyState(t *testing.T, r *KeyringReconciler) (*corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial) {
	t.Helper()

	var secret corev1.Secret

	deps := fixtureDependencies[r.authority]
	if err := deps.reader.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}, &secret); err != nil {
		t.Fatal(err)
	}

	b, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	if err != nil {
		t.Fatal(err)
	}

	var state RotationState
	if err := json.Unmarshal(secret.Data["rotation.json"], &state); err != nil {
		t.Fatal(err)
	}

	var material issuerMaterial
	if err := json.Unmarshal(secret.Data["issuer.json"], &material); err != nil {
		t.Fatal(err)
	}

	return &secret, b, state, material
}

// Fault injection is a Kubernetes dependency supplied at construction, not an
// authority mutation hook. Tests may change the transport under that dependency.
type fixtureDependency struct {
	client.Client
	reader client.Reader
	now    func() time.Time
}

func (d *fixtureDependency) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return d.reader.Get(ctx, key, obj, opts...)
}

func (d *fixtureDependency) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return d.reader.List(ctx, list, opts...)
}

var fixtureDependencies = map[*authority.Authority]*fixtureDependency{}

func assembleFixture(cfg fixtureConfig, c client.Client, reader client.Reader) *Application {
	d := &fixtureDependency{Client: c, reader: reader, now: time.Now}
	a := Assemble(cfg, d, d)
	// Supply a clock through construction; no setter is exposed by authority.
	owner := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d, Now: func() time.Time { return d.now() }})
	a.authority = owner
	a.Topology.authority = owner
	a.Keyring.authority = owner
	a.Server.authority = owner
	a.Replication.authority = owner
	a.Lifecycle.authority = owner
	a.Topology.Client = c
	a.Topology.APIReader = reader
	a.Replication.Client = c
	a.Replication.APIReader = reader
	fixtureDependencies[owner] = d
	fixtureConfigs[owner] = cfg

	return a
}

func decodeIssuedResponse(t *testing.T, encoded []byte) wire.BootstrapResponse {
	t.Helper()

	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func issuanceRequest(t *testing.T, r *KeyringReconciler) (NodeIdentity, wire.BootstrapRequest, ed25519.PublicKey) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

	return NodeIdentity{}, wire.BootstrapRequest{SchemaVersion: 1, Cluster: r.Config.Cluster, Enrollment: testOtherUID, CSRDER: csr, Shares: wire.DefaultShares}, pub
}

func fixtureIdentity(t *testing.T, f *servingFixture) NodeIdentity {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)

	identity, err := f.a.authority.Authenticate(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}

	return identity
}

func invalidateFixtureTrust(t *testing.T, f *servingFixture) {
	t.Helper()
	secret, _, _, _ := keyState(t, f.a.Keyring)

	secret.Data["bundle.json"] = []byte(`{}`)
	if err := f.a.Topology.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if err := f.a.authority.Observe(t.Context()); err == nil {
		t.Fatal("invalid credential observation succeeded")
	}
}

func readVersion(ctx context.Context, reader client.Reader, cfg fixtureConfig) (*corev1.ConfigMap, VersionRecord, error) {
	if err := authority.New(cfg.authorityConfig(), authority.Dependencies{Reader: reader}).Recover(ctx, nil); err != nil {
		return nil, VersionRecord{}, err
	}

	var cm corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}, &cm); err != nil {
		return nil, VersionRecord{}, err
	}

	seq, err := strconv.ParseUint(cm.Data["sequence"], 10, 64)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	members, err := strconv.ParseUint(cm.Data["membership_version"], 10, 64)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	return &cm, VersionRecord{Cluster: wire.ClusterID(cm.Data["cluster"]), Sequence: wire.Sequence(seq), MembershipVersion: wire.MembershipVersion(members), ContentHash: cm.Data["content_hash"], MembershipHash: cm.Data["membership_hash"]}, nil
}

func configureFixtureAge(t *testing.T, f *servingFixture, age time.Duration) {
	t.Helper()

	cfg := f.a.Topology.Config
	cfg.SnapshotMaxAge = age
	d := fixtureDependencies[f.a.authority]
	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d, Now: func() time.Time { return d.now() }})
	f.a.authority = a
	f.a.Topology.authority = a
	f.a.Keyring.authority = a
	f.a.Server.authority = a
	f.a.Replication.authority = a
	f.a.Lifecycle.authority = a

	fixtureDependencies[a] = d

	fixtureConfigs[a] = cfg
	if err := a.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}

	reconcileTopology(t, f.a.Topology, f.ctx)
}

func replaceFixtureCredentials(t *testing.T, f *servingFixture) {
	t.Helper()
	other := newServingFixture(t)
	candidate, _, _, _ := keyState(t, other.a.Keyring)
	current, bundle, _, _ := keyState(t, f.a.Keyring)

	var (
		replacement wire.KeyringBundle
		err         error
	)

	replacement, err = wire.DecodeBundle(bytes.NewReader(candidate.Data["bundle.json"]))
	if err != nil {
		t.Fatal(err)
	}

	replacement.Generation = bundle.Generation + 1

	candidate.Data["bundle.json"], err = wire.EncodeBundle(replacement)
	if err != nil {
		t.Fatal(err)
	}

	current.Data = candidate.Data
	if err := f.a.Topology.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}

	if err := f.a.authority.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}
}

var withdrawnSecrets = map[*authority.Authority]*corev1.Secret{}

func withdrawServerTrust(t *testing.T, s *Server) {
	t.Helper()

	d := fixtureDependencies[s.authority]

	var secret corev1.Secret
	if err := d.Client.Get(t.Context(), client.ObjectKey{Namespace: fixtureConfigs[s.authority].Namespace, Name: fixtureConfigs[s.authority].CredentialsSecretName}, &secret); err != nil {
		t.Fatal(err)
	}

	if withdrawnSecrets[s.authority] == nil {
		withdrawnSecrets[s.authority] = secret.DeepCopy()
	}

	secret.Data["bundle.json"] = []byte(`{}`)
	if err := d.Update(t.Context(), &secret); err != nil {
		t.Fatal(err)
	}

	if _, err := s.authority.ReconcileCredentials(t.Context()); err == nil {
		t.Fatal("invalid trust accepted")
	}
}

func restoreServerTrust(t *testing.T, s *Server) {
	t.Helper()

	d := fixtureDependencies[s.authority]

	saved := withdrawnSecrets[s.authority]
	if saved == nil {
		return
	}

	var secret corev1.Secret
	if err := d.Client.Get(t.Context(), client.ObjectKeyFromObject(saved), &secret); err != nil {
		t.Fatal(err)
	}

	secret.Data = saved.DeepCopy().Data
	if err := d.Update(t.Context(), &secret); err != nil {
		t.Fatal(err)
	}

	if _, err := s.authority.ReconcileCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func withdrawPublication(t *testing.T, r *TopologyReconciler) func() {
	t.Helper()

	cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	saved := cm.DeepCopy()

	cm.Data["sequence"] = "0"
	if err := r.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}

	if _, err := r.authority.PublishTopology(t.Context(), r.observeTopology); err == nil {
		t.Fatal("invalid publication accepted")
	}

	return func() {
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
			t.Fatal(err)
		}

		cm.Data = saved.Data
		if err := r.Update(t.Context(), cm); err != nil {
			t.Fatal(err)
		}
	}
}

func parseSigning(m signingMaterial) (*x509.Certificate, ed25519.PrivateKey, error) {
	cert, err := x509.ParseCertificate(m.Certificate)
	if err != nil {
		return nil, nil, err
	}

	key, err := x509.ParsePKCS8PrivateKey(m.PrivateKey)
	if err != nil {
		return nil, nil, err
	}

	return cert, key.(ed25519.PrivateKey), nil
}

func waitFixturePublication(ctx context.Context, a *authority.Authority, after wire.Sequence) (*authority.PublicationHandle, error) {
	for {
		p, changed, err := a.CurrentAndSubscribe()
		if err != nil {
			return nil, err
		}

		if p.Sequence() > after {
			return p, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func rootID(der []byte) string { sum := sha256.Sum256(der); return hex.EncodeToString(sum[:]) }

func generateIssuer(now time.Time, cfg fixtureConfig) ([]byte, []byte, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(cfg.Rotation.Interval + cfg.Rotation.PrepareFor + cfg.Rotation.RetainFor + 2*cfg.CertificateLifetime), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		return nil, nil, err
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(key)

	return der, encoded, err
}

func largeFixturePublication(t *testing.T, f *servingFixture) {
	t.Helper()

	members := make(AcceptedMembers, 50000)

	for i := range 50000 {
		id := wire.NodeID(fmt.Sprintf("33333333-3333-4333-8333-%012d", i))
		members[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}
	}

	replicationSmokePublish(t, f.ctx, f.a.Topology, members)
}

func rotateFixtureTrust(t *testing.T, f *servingFixture) {
	t.Helper()
	secret, bundle, _, _ := keyState(t, f.a.Keyring)
	bundle.Generation++

	encoded, err := wire.EncodeBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	secret.Data["bundle.json"] = encoded
	if err := f.a.Topology.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if _, err := f.a.authority.ReconcileCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type teardownListener struct {
	net.Listener
	accept func() (net.Conn, error)
	close  func() error
}

func (l teardownListener) Accept() (net.Conn, error) { return l.accept() }

func (l teardownListener) Close() error { return l.close() }

func TestServeTeardownErrorsAndLateAccept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		s := New(Config{Limits: Limits{ShutdownTimeout: time.Second}}, nil, nil, NewLifecycle(nil), nil)

		accepted, peer := net.Pipe()
		defer peer.Close()
		defer accepted.Close()

		closing := make(chan struct{})
		acceptErr := errors.New("accept failed during cancellation")
		closeErr := errors.New("listener close failed")
		calls := 0
		listener := teardownListener{
			accept: func() (net.Conn, error) {
				if accepted != nil {
					// Return a connection only after the force-close sweep, while
					// net/http is closing the listener and waiting for Serve.
					<-closing

					conn := accepted
					accepted = nil

					return conn, nil
				}

				return nil, acceptErr
			},
			close: func() error {
				calls++

				close(closing)

				return closeErr
			},
		}
		done := make(chan error, 1)

		go func() { done <- s.serve(ctx, listener, &tls.Config{}) }()

		synctest.Wait()
		cancel()
		synctest.Wait()

		// net/http reports ErrServerClosed once Close starts, even if Accept
		// itself failed. The listener's close failure must still be returned.
		if err := <-done; !errors.Is(err, closeErr) {
			t.Fatalf("teardown error: %v", err)
		}

		if calls != 1 || s.Lifecycle.serving {
			t.Fatalf("close calls = %d, serving = %v", calls, s.Lifecycle.serving)
		}

		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("late connection was not closed: %v", err)
		}
	})
}

func TestServeTeardownCompletionBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		s := New(Config{Limits: Limits{ShutdownTimeout: time.Second}}, nil, nil, NewLifecycle(nil), nil)
		unblock := make(chan struct{})

		release := sync.OnceFunc(func() { close(unblock) })
		defer release()

		listener := teardownListener{
			accept: func() (net.Conn, error) { <-unblock; return nil, net.ErrClosed },
			close:  func() error { <-unblock; return nil },
		}
		done := make(chan error, 1)

		go func() { done <- s.serve(ctx, listener, &tls.Config{}) }()

		synctest.Wait()
		cancel()

		start := time.Now()

		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked teardown: %v", err)
		}

		if elapsed := time.Since(start); elapsed != s.config.Limits.ShutdownTimeout {
			t.Fatalf("completion wait = %s", elapsed)
		}

		if s.Lifecycle.serving {
			t.Fatal("timed-out server still serving ready")
		}

		release()
		synctest.Wait()
	})
}

func TestServeTeardownClosesSlowTLSWrite(t *testing.T) {
	for _, cause := range []string{"cancellation", "accept failure"} {
		t.Run(cause, func(t *testing.T) {
			f := newServingFixture(t)
			f.configureServer(func(c *Config) {
				c.Limits.WriteTimeout = time.Minute
				c.Limits.ShutdownTimeout = time.Second
			})
			largeFixturePublication(t, f)

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)

			t.Cleanup(func() { _ = listener.Close() })

			done := make(chan error, 1)

			config := f.a.Server.tlsConfigWithCertificate(f.ctx, func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &f.serverCertificate, nil
			})

			go func() { done <- f.a.Server.serve(f.ctx, listener, config) }()

			conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
			require.NoError(t, err)

			defer conn.Close()

			_, err = io.WriteString(conn, "GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\n\r\n")
			require.NoError(t, err)
			require.Eventually(t, func() bool { return len(f.a.Server.writes) != 0 }, 5*time.Second, time.Millisecond, "write not admitted")
			// Keep the peer open without reading. Teardown must release the
			// socket and admission before the much longer write deadline.
			if cause == "cancellation" {
				f.cancel()
			} else if err := listener.Close(); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-done:
				if cause == "cancellation" && err != nil || cause == "accept failure" && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("serve result: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TLS teardown blocked")
			}

			awaitServerPolls(t, f.a.Server, 0)

			require.Empty(t, f.a.Server.writes, "teardown retained admission")
			require.Error(t, f.a.Server.Ready(nil), "teardown retained readiness")
		})
	}
}

func TestServingChainFreezesBeforeFirstRequest(t *testing.T) {
	for _, boundary := range []string{"handler", "tls"} {
		t.Run(boundary, func(t *testing.T) {
			f := newServingFixture(t)
			s := f.a.Server
			// Pre-use overrides now belong to construction, not mutable engines.
			cfg := f.a.Topology.Config
			cfg.CertificateLifetime = 2 * time.Minute
			cfg.SnapshotMaxAge = 30 * time.Second
			d := fixtureDependencies[f.a.authority]

			s.authority = authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d})
			require.NoError(t, s.authority.Observe(f.ctx))

			f.a.Replication.Config = cfg

			want := cfg
			input := s.config
			s = New(input, s.writer, s.authority, s.Lifecycle, s.Leader)
			s.installTestServingCertificate(f.serverCertificate)

			wantServer := input

			var handler http.Handler
			if boundary == "handler" {
				handler = s.Handler()
			} else {
				s.tlsConfigWithCertificate(f.ctx, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &f.serverCertificate, nil })
			}
			// Mutate sequentially, before any request, without calling dependency
			// getters first: those calls would accidentally hide lazy freezing.
			cfg.DataplaneServiceAccount = "wrong-account"
			cfg.Cluster = ""
			cfg.CertificateLifetime = time.Second
			input.Limits.HeaderBytes = 1

			cfg.ControllerServiceAccount = "wrong-controller"

			if handler == nil {
				handler = s.Handler()
			}

			require.Equal(t, wantServer, s.config, "server did not freeze transport before exposure")
			request := bootstrapTestRequest(t, f.ctx, "", f.token, f.request)
			request.TLS = &tls.ConnectionState{HandshakeComplete: true}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			require.Equal(t, http.StatusOK, w.Code, "first request used post-exposure config")

			response := decodeIssuedResponse(t, w.Body.Bytes())

			leaf, err := x509.ParseCertificate(response.CertificateChain[0])
			if err != nil {
				t.Fatal(err)
			}

			require.Equal(t, want.Cluster, response.Cluster)
			require.Equal(t, want.CertificateLifetime+time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))
		})
	}
}

func TestLifecycleProcessContext(t *testing.T) {
	for _, source := range []string{"parent", "process", "child"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				process, stopProcess := context.WithCancel(t.Context())
				defer stopProcess()

				parent, stopParent := context.WithTimeout(t.Context(), time.Minute)
				defer stopParent()

				key := connectionKey{}
				parent = context.WithValue(parent, key, source)
				l := NewLifecycle(nil)
				l.process = process

				child, cancel := l.ProcessContext(parent)
				defer cancel()

				deadline, ok := child.Deadline()

				parentDeadline, _ := parent.Deadline()

				require.NoError(t, child.Err())
				require.Equal(t, source, child.Value(key))
				require.True(t, ok)
				require.Equal(t, parentDeadline, deadline)

				switch source {
				case "parent":
					stopParent()
				case "process":
					stopProcess()
				case "child":
					cancel()
				}

				synctest.Wait()

				if !errors.Is(child.Err(), context.Canceled) {
					t.Fatalf("child ignored %s cancellation: %v", source, child.Err())
				}

				if source != "process" && process.Err() != nil || source != "parent" && parent.Err() != nil {
					t.Fatal("child cancellation propagated to an independent parent")
				}
			})
		})
	}

	process, cancel := context.WithCancel(t.Context())
	cancel()

	for _, l := range []*Lifecycle{nil, NewLifecycle(nil), {process: process}} {
		child, stop := l.ProcessContext(t.Context())
		if !errors.Is(child.Err(), context.Canceled) {
			t.Fatal("absent or canceled process did not immediately cancel child")
		}

		stop()
	}
}

func TestLifecycleHTTPReadinessTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Server, bool)
	}{
		{name: "issuer", set: func(s *Server, ready bool) {
			if ready {
				restoreServerTrust(t, s)
			} else {
				withdrawServerTrust(t, s)
			}
		}},
		{name: "serving", set: func(s *Server, ready bool) { s.Lifecycle.SetServingReady(ready) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServingFixture(t)
			handler := f.a.Server.Handler()

			tc.set(f.a.Server, false)

			for _, step := range []struct {
				name  string
				ready bool
			}{
				{name: "initial false"},
				{name: "become ready", ready: true},
				{name: "remain ready", ready: true},
				{name: "withdraw readiness"},
				{name: "remain unready"},
				{name: "restore readiness", ready: true},
			} {
				t.Run(step.name, func(t *testing.T) {
					tc.set(f.a.Server, step.ready)

					r := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
					r.TLS = f.requestState(t)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)

					want := http.StatusServiceUnavailable
					if step.ready {
						want = http.StatusOK
					}

					responseBody(t, w.Result(), nil, want)
				})
			}
		})
	}
}

func TestLifecycleFollowerWithValidatedPublicationIsReady(t *testing.T) {
	r := initializedTopology(t)
	reconcileTopology(t, r, t.Context())

	if err := r.authority.PublicationReady(); err != nil {
		t.Fatal(err)
	}

	l := NewLifecycle(r.authority)
	l.process, l.synced = t.Context(), true
	l.SetServingReady(true)

	if err := l.Ready(nil); err != nil {
		t.Fatalf("follower with validated state must receive Service traffic: %v", err)
	}
}

func TestLifecycleGatesAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)

		l := NewLifecycle(r.authority)
		if l.NeedLeaderElection() || l.Ready(nil) == nil {
			t.Fatal("follower ready")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		syncCache := make(chan struct{})
		l.waitForCacheSync = func(ctx context.Context) bool {
			select {
			case <-ctx.Done():
				return false
			case <-syncCache:
				return true
			}
		}
		started := make(chan error, 1)

		go func() { started <- l.Start(ctx) }()

		l.SetServingReady(true)
		reconcileTopology(t, r, ctx)

		if l.Ready(nil) == nil {
			t.Fatal("ready before synchronized inputs")
		}

		close(syncCache)
		synctest.Wait()

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		restore := withdrawPublication(t, r)

		if l.Ready(nil) == nil {
			t.Fatal("ready without publication authority")
		}

		restore()
		reconcileTopology(t, r, ctx)
		l.SetServingReady(false)

		if l.Ready(nil) == nil {
			t.Fatal("ready without listener")
		}

		cancel()

		if err := <-started; err != nil {
			t.Fatal(err)
		}

		l.SetServingReady(true)

		if l.Ready(nil) == nil {
			t.Fatal("old process resurrected")
		}

		request, stop := l.ProcessContext(t.Context())
		defer stop()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request resurrected: %v", request.Err())
		}

		if err := l.Start(context.Background()); !errors.Is(err, wire.Conflict) {
			t.Fatalf("process restarted: %v", err)
		}
	})
}

func TestLifecycleRequiresPublicationAndHonorsRequestCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)
		l := NewLifecycle(r.authority)
		l.waitForCacheSync = func(context.Context) bool { return true }

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- l.Start(ctx) }()

		l.SetServingReady(true)
		synctest.Wait()

		requestCtx, stop := context.WithCancel(context.Background())
		stop()

		request, stopRequest := l.ProcessContext(requestCtx)
		defer stopRequest()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request ignores cancellation: %v", request.Err())
		}

		if err := l.Ready(nil); !errors.Is(err, wire.Unavailable) {
			t.Fatalf("ready without publication: %v", err)
		}

		reconcileTopology(t, r, ctx)

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		require.ErrorIs(t, l.Ready(nil), wire.Unavailable)

		beforeStartup := NewLifecycle(r.authority)
		beforeStartup.waitForCacheSync = func(ctx context.Context) bool { return ctx.Err() == nil }
		require.NoError(t, beforeStartup.Start(ctx))
		beforeStartup.SetServingReady(true)
		require.ErrorIs(t, beforeStartup.Ready(nil), wire.Unavailable, "canceled startup became ready")
	})
}

func TestLifecycleHTTPStartupAdmission(t *testing.T) {
	for _, scenario := range []string{"synchronized", "cache failed", "canceled during sync"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				l := NewLifecycle(f.a.authority)
				f.a.Server.Lifecycle = l
				l.SetServingReady(true)

				syncCache := make(chan struct{})
				l.waitForCacheSync = func(ctx context.Context) bool {
					select {
					case <-ctx.Done():
						return false
					case <-syncCache:
						return scenario == "synchronized"
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				done := make(chan error, 1)

				go func() { done <- l.Start(ctx) }()

				synctest.Wait()

				handler := f.a.Server.Handler()
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				before := httptest.NewRecorder()
				handler.ServeHTTP(before, request)
				responseBody(t, before.Result(), nil, http.StatusServiceUnavailable)

				if scenario == "canceled during sync" {
					cancel()
				} else {
					close(syncCache)
				}

				synctest.Wait()

				after := httptest.NewRecorder()
				handler.ServeHTTP(after, request)

				want := http.StatusServiceUnavailable
				if scenario == "synchronized" {
					want = http.StatusOK
				}

				responseBody(t, after.Result(), nil, want)

				cancel()

				err := <-done
				if scenario == "cache failed" {
					if !errors.Is(err, wire.Unavailable) {
						t.Fatalf("cache failure: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}

				stopped := httptest.NewRecorder()
				handler.ServeHTTP(stopped, request)
				responseBody(t, stopped.Result(), nil, http.StatusServiceUnavailable)
			})
		})
	}
}

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

// Fixed-certificate adapter for socket tests; production always uses the reloader.
func (s *Server) tlsConfig(ctx context.Context, certificate tls.Certificate) *tls.Config {
	r := s.installTestServingCertificate(certificate)
	return s.tlsConfigWithCertificate(ctx, r.getCertificate)
}

func (s *Server) installTestServingCertificate(certificate tls.Certificate) *servingCertificateReloader {
	validated, err := validateServingCertificate(certificate, time.Now())
	if err != nil {
		panic(err)
	}

	r := &servingCertificateReloader{}
	r.current.Store(validated)
	s.servingCertificate.Store(r)

	return r
}

func newServingFixture(t *testing.T) *servingFixture {
	t.Helper()
	a, status, token := authFixture(t)
	installReview(t, a, status, token)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, ctx)
	a.Lifecycle.mu.Lock()
	a.Lifecycle.process, a.Lifecycle.synced, a.Lifecycle.serving = ctx, true, true
	a.Lifecycle.mu.Unlock()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"attacker"}, Subject: pkix.Name{CommonName: "attacker"}}, key)
	if err != nil {
		t.Fatal(err)
	}

	request := wire.BootstrapRequest{SchemaVersion: 1, Cluster: a.Topology.Config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares}
	authRequest := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	authRequest.Header.Set("Authorization", "Bearer "+token)

	identity, err := a.authority.Authenticate(ctx, authRequest)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := a.authority.Issue(ctx, identity, request)
	if err != nil {
		t.Fatal(err)
	}

	response := decodeIssuedResponse(t, encoded)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{a.Server.config.ReplicationServerName}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

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
	a.Server.installTestServingCertificate(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})

	return &servingFixture{a: a, token: token, key: key, request: request, certificate: tls.Certificate{Certificate: response.CertificateChain, PrivateKey: key}, serverCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots, ctx: ctx, cancel: cancel}
}

// configureServer replaces the unstarted fixture server with constructor inputs.
func (f *servingFixture) configureServer(change func(*Config)) {
	s := f.a.Server
	cfg := s.config
	change(&cfg)
	f.a.Server = New(cfg, s.writer, s.authority, s.Lifecycle, s.Leader)
	f.a.Server.servingCertificate.Store(s.servingCertificate.Load())
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

func TestOperationalStartCancellationAndTLSFiles(t *testing.T) {
	f := newServingFixture(t)
	dir := t.TempDir()

	f.configureServer(func(c *Config) {
		c.ControlAddress = "127.0.0.1:0"
		c.TLSCertificateFile = filepath.Join(dir, "tls.crt")
		c.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	})
	writeServingTestPair(t, dir, f.serverCertificate)

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

func TestHTTPWriteBootstrapAndGlobalAdmission(t *testing.T) {
	f := newServingFixture(t)
	f.configureServer(func(c *Config) {
		c.Limits.MaxPolls = 1
		c.Limits.MaxConcurrentWrites = 1
		c.Limits.MaxConcurrentBootstrap = 1
	})
	handler := f.a.Server.Handler()
	r := httptest.NewRequest("GET", wire.SnapshotPath, nil)

	r.TLS = f.requestState(t)
	for _, resource := range []string{"write", "bootstrap", "headers"} {
		t.Run(resource, func(t *testing.T) {
			request := r.Clone(f.ctx)

			switch resource {
			case "write":
				take(f.a.Server.writes)
				defer release(f.a.Server.writes)
			case "bootstrap":
				take(f.a.Server.bootstrapSlots)
				defer release(f.a.Server.bootstrapSlots)

				request.Method = "POST"
				request.URL.Path = wire.BootstrapPath
			case "headers":
				request.Header.Set("X-Large", strings.Repeat("a", f.a.Server.config.Limits.HeaderBytes))
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
