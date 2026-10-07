// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestStartupGuards(t *testing.T) {
	f := newServingFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	for _, tc := range []struct {
		name   string
		mutate func(*Server)
		want   error
	}{
		{"invalid limits", func(s *Server) { s.config.Limits.MaxPolls = 0 }, wire.InvalidRequest},
		{"missing address", func(s *Server) { s.config.ControlAddress = "" }, wire.InvalidRequest},
		{"missing lifecycle", func(s *Server) { s.Lifecycle = nil }, wire.Unavailable},
		{"missing authority", func(s *Server) { s.authority = nil }, wire.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(f.a.Server.config, f.a.Server.writer, f.a.authority, f.a.Lifecycle, f.a.Replication)
			tc.mutate(s)
			require.ErrorIs(t, s.Start(canceled), tc.want)
		})
	}

	s := New(f.a.Server.config, nil, nil, nil, nil)
	require.False(t, s.NeedLeaderElection())
	_, err := s.TLSConfig(canceled)
	require.ErrorIs(t, err, wire.Unavailable)

	s = New(Config{}, nil, f.a.authority, nil, nil)
	_, err = s.TLSConfig(canceled)
	require.ErrorIs(t, err, wire.InvalidRequest)

	lifecycle := NewLifecycle(f.a.authority)
	lifecycle.SetCacheSync(func(ctx context.Context) bool { return ctx.Err() == nil })
	require.NoError(t, lifecycle.Start(canceled))
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { flushResponse(canceled, httptest.NewRecorder()) })
}

func servingTestCertificate(t *testing.T, serial int64, before, after time.Time, parent *tls.Certificate, ca bool) tls.Certificate {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fmt.Sprint(serial)}, NotBefore: before, NotAfter: after, DNSNames: []string{"racer-controller.racer.svc"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: ca, BasicConstraintsValid: true}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	}

	issuer, signer := template, key
	if parent != nil {
		issuer = parent.Leaf
		signer = parent.PrivateKey.(ed25519.PrivateKey)
	}

	der, err := x509.CreateCertificate(rand.Reader, template, issuer, pub, signer)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	if parent != nil {
		certificate.Certificate = append(certificate.Certificate, parent.Certificate...)
	}

	return certificate
}

func writeServingTestPair(t *testing.T, dir string, certificate tls.Certificate) {
	t.Helper()

	var chain []byte
	for _, der := range certificate.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{"tls.crt": chain, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServingCertificateValidation(t *testing.T) {
	now := time.Now()
	root := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	valid := servingTestCertificate(t, 2, now.Add(-time.Minute), now.Add(time.Minute), &root, false)

	other := servingTestCertificate(t, 3, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	for _, tc := range []struct {
		name        string
		certificate tls.Certificate
		valid       bool
	}{
		{"valid chain", valid, true},
		{"leaf only", servingTestCertificate(t, 4, now.Add(-time.Minute), now.Add(time.Minute), nil, false), true},
		{"expired", servingTestCertificate(t, 5, now.Add(-time.Hour), now.Add(-time.Second), &root, false), false},
		{"future", servingTestCertificate(t, 6, now.Add(time.Minute), now.Add(time.Hour), &root, false), false},
		{"wrong chain", tls.Certificate{Certificate: [][]byte{valid.Certificate[0], other.Certificate[0]}, PrivateKey: valid.PrivateKey}, false},
		{"malformed chain", tls.Certificate{Certificate: [][]byte{valid.Certificate[0], {1, 2, 3}}, PrivateKey: valid.PrivateKey}, false},
		{"CA leaf", root, false},
		{"expired issuer", servingTestCertificate(t, 7, now.Add(-time.Minute), now.Add(time.Minute), certificatePointer(servingTestCertificate(t, 8, now.Add(-time.Hour), now.Add(-time.Second), nil, true)), false), false},
		{"future issuer", servingTestCertificate(t, 9, now.Add(-time.Minute), now.Add(time.Minute), certificatePointer(servingTestCertificate(t, 10, now.Add(time.Minute), now.Add(time.Hour), nil, true)), false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeServingTestPair(t, dir, valid)

			r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
			if err != nil {
				t.Fatal(err)
			}

			initial := r.current.Load()

			writeServingTestPair(t, dir, tc.certificate)

			_, err = newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}

			err = r.reload()
			if (err == nil) != tc.valid || !tc.valid && r.current.Load() != initial {
				t.Fatal("replacement validation or last-valid retention failed")
			}
		})
	}
}

func certificatePointer(c tls.Certificate) *tls.Certificate { return &c }

func TestServingCertificateChainOnlyRotation(t *testing.T) {
	now := time.Now()
	old := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	current := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	leaf := servingTestCertificate(t, 3, now.Add(-time.Minute), now.Add(time.Hour), &current, false)
	bridge, err := x509.CreateCertificate(rand.Reader, current.Leaf, old.Leaf, current.Leaf.PublicKey, old.PrivateKey)
	require.NoError(t, err)

	compatible := leaf
	compatible.Certificate = [][]byte{leaf.Certificate[0], bridge, old.Certificate[0]}
	dir := t.TempDir()
	writeServingTestPair(t, dir, compatible)
	r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	require.NoError(t, err)
	testCachedHandshake(t, r, now, old.Leaf, true, 3)

	initial := r.current.Load()

	writeServingTestPair(t, dir, leaf)
	require.NoError(t, r.reload())
	require.NotSame(t, initial, r.current.Load())
	selected, err := r.getCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, compatible.Certificate[0], selected.Certificate[0], "leaf must remain unchanged")
	require.Equal(t, leaf.Certificate, selected.Certificate, "chain-only update was ignored")
	testCachedHandshake(t, r, now, current.Leaf, true, 2)
	testCachedHandshake(t, r, now, old.Leaf, false, 0)

	bad := compatible
	bad.Certificate = [][]byte{leaf.Certificate[0], old.Certificate[0]}
	last := r.current.Load()

	writeServingTestPair(t, dir, bad)
	require.Error(t, r.reload(), "same-leaf invalid chain bypassed validation")
	require.Same(t, last, r.current.Load())
	testCachedHandshake(t, r, now, current.Leaf, true, 2)
}

func TestServerReadinessTracksCachedServingCertificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		s := f.a.Server
		s.servingCertificate.Store(nil)

		if s.Ready(nil) == nil {
			t.Fatal("ready without serving certificate initialization")
		}

		r := &servingCertificateReloader{}
		s.servingCertificate.Store(r)

		if s.Ready(nil) == nil {
			t.Fatal("ready with empty certificate cache")
		}

		dir := t.TempDir()
		now := time.Now()
		first := servingTestCertificate(t, 1, now.Add(-time.Minute), now.Add(2*time.Second), nil, false)
		writeServingTestPair(t, dir, first)

		r.certificateFile, r.keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
		if err := r.reload(); err != nil {
			t.Fatal(err)
		}

		if err := s.Ready(nil); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(r.certificateFile, []byte("broken projection"), 0o600); err != nil {
			t.Fatal(err)
		}

		if r.reload() == nil {
			t.Fatal("accepted invalid reload")
		}

		if err := s.Ready(nil); err != nil {
			t.Fatal("lost valid last-good certificate", err)
		}

		time.Sleep(2 * time.Second)

		if s.Ready(nil) == nil {
			t.Fatal("ready with expired cached certificate")
		}

		second := servingTestCertificate(t, 2, now.Add(-time.Minute), now.Add(time.Hour), nil, false)
		writeServingTestPair(t, dir, second)

		if err := r.reload(); err != nil {
			t.Fatal(err)
		}

		if err := s.Ready(nil); err != nil {
			t.Fatal("reload did not restore readiness", err)
		}
	})
}

func TestServerReadinessUsesUsableChainPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		now := time.Now()
		old := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Second), nil, true)
		current := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
		leaf := servingTestCertificate(t, 3, now.Add(-time.Minute), now.Add(time.Minute), &current, false)
		bridge := *current.Leaf
		bridge.NotAfter = old.Leaf.NotAfter

		der, err := x509.CreateCertificate(rand.Reader, &bridge, old.Leaf, current.Leaf.PublicKey, old.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}

		leaf.Certificate = [][]byte{leaf.Certificate[0], der, old.Certificate[0]}

		r := f.a.Server.installTestServingCertificate(leaf)
		if err := f.a.Server.Ready(nil); err != nil {
			t.Fatal(err)
		}

		time.Sleep(time.Second)

		if err := f.a.Server.Ready(nil); err != nil {
			t.Fatal("optional suffix expiration withdrew readiness", err)
		}

		selected, err := r.getCertificate(nil)
		if err != nil || len(selected.Certificate) != 1 {
			t.Fatal("handshake did not select usable prefix", err)
		}
	})
}

func TestServingCertificateProjectionAndRecovery(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, false)

	second := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	for name, certificate := range map[string]tls.Certificate{"one": first, "two": second} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.Mkdir(path, 0o700))

		writeServingTestPair(t, path, certificate)
	}

	project := func(generation string) {
		t.Helper()

		require.NoError(t, os.Symlink(generation, filepath.Join(dir, "..next")))
		require.NoError(t, os.Rename(filepath.Join(dir, "..next"), filepath.Join(dir, "..data")))
	}
	project("one")

	for _, name := range []string{"tls.crt", "tls.key"} {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)))
	}

	r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}

	project("two")

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	if certificate, err := r.getCertificate(nil); err != nil || certificate.Leaf.SerialNumber.Int64() != 2 {
		t.Fatal("atomic projection did not replace certificate")
	}

	project("one")

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	initial := r.current.Load()
	// Deliberately pin the key to the old generation. Reject even when both
	// generations reuse the same key and X509KeyPair alone would accept them.
	second.PrivateKey = first.PrivateKey
	second.Certificate = first.Certificate
	writeServingTestPair(t, filepath.Join(dir, "two"), second)

	require.NoError(t, os.Remove(filepath.Join(dir, "tls.key")))

	require.NoError(t, os.Symlink("one/tls.key", filepath.Join(dir, "tls.key")))

	project("two")

	require.Error(t, r.reload(), "mixed generation accepted")
	require.Same(t, initial, r.current.Load())

	require.NoError(t, os.Remove(filepath.Join(dir, "tls.key")))

	require.NoError(t, os.Symlink("..data/tls.key", filepath.Join(dir, "tls.key")))

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	require.NotSame(t, initial, r.current.Load(), "projection did not recover")

	last := r.current.Load()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "two/tls.crt"), []byte("malformed"), 0o600))

	if err := r.reload(); err == nil || r.current.Load() != last {
		t.Fatal("malformed update replaced certificate")
	}

	require.NoError(t, os.Remove(filepath.Join(dir, "two/tls.key")))

	if err := r.reload(); err == nil || r.current.Load() != last {
		t.Fatal("missing update replaced certificate")
	}

	writeServingTestPair(t, filepath.Join(dir, "two"), first)

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}
}

func TestServingCertificateStandaloneTornPair(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	second := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	writeServingTestPair(t, dir, first)

	r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}

	initial := r.current.Load()
	torn := second
	torn.PrivateKey = first.PrivateKey
	writeServingTestPair(t, dir, torn)

	if err := r.reload(); err == nil || r.current.Load() != initial {
		t.Fatal("torn pair accepted")
	}

	writeServingTestPair(t, dir, second)

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	if certificate, err := r.getCertificate(nil); err != nil || certificate.Leaf.SerialNumber.Int64() != 2 {
		t.Fatal("pair did not recover")
	}

	last := r.current.Load()

	file, err := os.OpenFile(filepath.Join(dir, "tls.crt"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, writeErr := file.WriteString("-----BEGIN CERTIFICATE-----\ntruncated")

	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append: %v, close: %v", writeErr, closeErr)
	}

	if err := r.reload(); err == nil || r.current.Load() != last {
		t.Fatal("truncated chain replaced certificate")
	}
}

func TestServingCertificateExpirationAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		now := time.Now()
		certificate := servingTestCertificate(t, 1, now.Add(-time.Minute), now.Add(2*time.Second), nil, false)
		writeServingTestPair(t, dir, certificate)

		r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() { defer close(done); r.run(ctx, time.Second) }()
		// Bad files do not invalidate a still-valid cached certificate.
		if err := os.Remove(filepath.Join(dir, "tls.crt")); err != nil {
			t.Fatal(err)
		}

		if _, err := r.getCertificate(nil); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Second)

		if _, err := r.getCertificate(nil); err == nil {
			t.Fatal("expired cached certificate served")
		}

		cancel()
		<-done

		last := r.current.Load()

		writeServingTestPair(t, dir, servingTestCertificate(t, 2, now, now.Add(time.Hour), nil, false))
		time.Sleep(2 * time.Second)

		if r.current.Load() != last {
			t.Fatal("reload continued after cancellation")
		}
	})
}

func TestServingTLSConfigInitialFailure(t *testing.T) {
	f := newServingFixture(t)
	dir := t.TempDir()

	f.configureServer(func(c *Config) {
		c.TLSCertificateFile = filepath.Join(dir, "tls.crt")
		c.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	})

	if _, err := f.a.Server.TLSConfig(f.ctx); err == nil {
		t.Fatal("missing initial pair accepted")
	}

	writeServingTestPair(t, dir, servingTestCertificate(t, 2, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour), nil, false))

	if _, err := f.a.Server.TLSConfig(f.ctx); err == nil {
		t.Fatal("future initial pair accepted")
	}

	writeServingTestPair(t, dir, f.serverCertificate)

	if _, err := f.a.Server.TLSConfig(context.Background()); err == nil {
		t.Fatal("unowned reload lifetime accepted")
	}

	f.cancel()

	if _, err := f.a.Server.TLSConfig(f.ctx); err == nil {
		t.Fatal("canceled reload lifetime accepted")
	}
}

func TestServingTLSLiveReloadPreservesConnectionsAndPolls(t *testing.T) {
	f := newServingFixture(t)
	dir := t.TempDir()

	f.configureServer(func(c *Config) {
		c.TLSCertificateFile = filepath.Join(dir, "tls.crt")
		c.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	})

	now := time.Now()
	root := servingTestCertificate(t, 10, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	first := servingTestCertificate(t, 11, now.Add(-time.Minute), now.Add(time.Hour), &root, false)
	// The replacement uses a new CA cross-signed by the old root. A client
	// retaining only the old root must still complete fresh TLS handshakes.
	newRoot := servingTestCertificate(t, 20, now.Add(-time.Hour), now.Add(time.Hour), nil, true)

	crossDER, err := x509.CreateCertificate(rand.Reader, newRoot.Leaf, root.Leaf, newRoot.Leaf.PublicKey, root.PrivateKey)
	require.NoError(t, err)

	second := servingTestCertificate(t, 12, now.Add(-time.Minute), now.Add(time.Hour), &newRoot, false)
	second.Certificate = [][]byte{second.Certificate[0], crossDER, root.Certificate[0]}
	f.roots = x509.NewCertPool()
	f.roots.AddCert(root.Leaf)
	writeServingTestPair(t, dir, first)

	config, err := f.a.Server.TLSConfig(f.ctx)
	require.NoError(t, err)

	s := httptest.NewUnstartedServer(f.a.Server.Handler())
	s.Config.ConnContext = connectionContext
	s.TLS = config
	s.StartTLS()
	t.Cleanup(s.Close)
	peer := f.client(t, &f.certificate)
	response, err := peer.Get(s.URL + wire.SnapshotPath)
	body := responseBody(t, response, err, http.StatusOK)

	publication, err := wire.DecodePublication(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	require.Equal(t, int64(11), response.TLS.PeerCertificates[0].SerialNumber.Int64(), "wrong initial certificate")

	type result struct {
		response *http.Response
		err      error
	}

	done := make(chan result, 1)

	go func() {
		pollResponse, pollErr := peer.Get(fmt.Sprintf("%s%s?after=%d", s.URL, wire.SnapshotPath, publication.Sequence))
		done <- result{pollResponse, pollErr}
	}()

	awaitServerPolls(t, f.a.Server, 1)
	writeServingTestPair(t, dir, second)
	awaitServingSerial(t, f, s.URL, 12)

	select {
	case result := <-done:
		if result.response != nil {
			result.response.Body.Close()
		}

		t.Fatalf("rotation interrupted long poll: %v", result.err)
	default:
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
	case result := <-done:
		responseBody(t, result.response, result.err, http.StatusOK)

		require.Equal(t, int64(11), result.response.TLS.PeerCertificates[0].SerialNumber.Int64(), "long poll reconnected")
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not complete")
	}

	reused := false
	ctx := httptrace.WithClientTrace(f.ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+wire.SnapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err = peer.Do(req)
	responseBody(t, response, err, http.StatusOK)

	require.True(t, reused, "persistent connection replaced")
	require.Equal(t, int64(11), response.TLS.PeerCertificates[0].SerialNumber.Int64())
}

func awaitServingSerial(t *testing.T, f *servingFixture, endpoint string, serial int64) {
	t.Helper()
	fresh := f.client(t, nil)
	fresh.Transport.(*http.Transport).DisableKeepAlives = true
	deadline := time.Now().Add(5 * time.Second)

	for {
		response, err := fresh.Get(endpoint + wire.SnapshotPath)
		responseBody(t, response, err, http.StatusUnauthorized)

		if response.TLS.PeerCertificates[0].SerialNumber.Int64() == serial {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("new handshakes did not see replacement")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestServingCertificateConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	second := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	writeServingTestPair(t, dir, first)

	r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				certificate, err := r.getCertificate(nil)
				if err != nil || certificate.Leaf.SerialNumber.Int64() < 1 || certificate.Leaf.SerialNumber.Int64() > 2 {
					t.Error("invalid concurrent certificate")
				}
			}
		})
	}

	for range 10 {
		for _, certificate := range []tls.Certificate{second, first} {
			writeServingTestPair(t, dir, certificate)

			if err := r.reload(); err != nil {
				t.Fatal(err)
			}
		}
	}

	wg.Wait()
}

func TestServingTLSRejectsExpiredCachedCertificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		dir := t.TempDir()
		now := time.Now()
		certificate := servingTestCertificate(t, 1, now.Add(-time.Minute), now.Add(time.Second), nil, false)
		writeServingTestPair(t, dir, certificate)

		r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatal(err)
		}

		config := f.a.Server.tlsConfigWithCertificate(f.ctx, r.getCertificate)
		// Advance past expiration without any reload. Even a client disabling
		// verification cannot make the server disclose an expired cached chain.
		time.Sleep(time.Second)

		serverSide, clientSide := net.Pipe()
		defer serverSide.Close()
		defer clientSide.Close()

		done := make(chan error, 1)

		go func() { done <- tls.Server(serverSide, config).HandshakeContext(f.ctx) }()

		peer := tls.Client(clientSide, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}) //nolint:gosec // Verify rejection by the server, not the client.
		if err := peer.HandshakeContext(f.ctx); err == nil {
			t.Fatal("expired server certificate accepted")
		}

		if err := <-done; err == nil {
			t.Fatal("server completed expired handshake")
		}

		if len(peer.ConnectionState().PeerCertificates) != 0 {
			t.Fatal("expired chain sent to client")
		}
	})
}

func TestServingTLSCompatibilitySuffixExpiration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := servingTestCertificate(t, 100, now.Add(-27*24*time.Hour), now.Add(24*time.Hour), nil, true)
	current := servingTestCertificate(t, 101, now.Add(-time.Hour), now.Add(28*24*time.Hour), nil, true)
	leaf := servingTestCertificate(t, 102, now.Add(-time.Minute), now.Add(7*24*time.Hour), &current, false)
	bridgeTemplate := *current.Leaf
	bridgeTemplate.NotAfter = old.Leaf.NotAfter

	bridgeDER, err := x509.CreateCertificate(rand.Reader, &bridgeTemplate, old.Leaf, current.Leaf.PublicKey, old.PrivateKey)
	require.NoError(t, err)

	leaf.Certificate = [][]byte{leaf.Certificate[0], bridgeDER, old.Certificate[0]}
	dir := t.TempDir()
	writeServingTestPair(t, dir, leaf)

	r := &servingCertificateReloader{certificateFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key")}
	require.NoError(t, r.reloadAt(now))

	initial := r.current.Load()
	// Remove source files: selection after the frozen expiration boundary must
	// work using only immutable cached, prevalidated chain prefixes.
	require.NoError(t, os.Remove(r.certificateFile))
	require.NoError(t, os.Remove(r.keyFile))

	handshake := func(at time.Time, root *x509.Certificate, want bool, length int) {
		t.Helper()
		testCachedHandshake(t, r, at, root, want, length)
	}
	handshake(now, old.Leaf, true, 3)
	handshake(now, current.Leaf, true, 3)
	after := old.Leaf.NotAfter
	handshake(after, current.Leaf, true, 1)
	handshake(after, old.Leaf, false, 0)
	handshake(leaf.Leaf.NotAfter, current.Leaf, false, 0)

	require.Same(t, initial, r.current.Load(), "handshake changed published certificate")
	// The operator normally sends leaf + bridges, omitting the old root.
	// Exercise that wire layout as well as the explicit-root layout above.
	withoutRoot := leaf
	withoutRoot.Certificate = leaf.Certificate[:2:2]
	writeServingTestPair(t, dir, withoutRoot)

	require.NoError(t, r.reloadAt(now))

	handshake(now, old.Leaf, true, 2)
	handshake(after, current.Leaf, true, 1)
	handshake(after, old.Leaf, false, 0)

	require.NoError(t, r.reloadAt(after), "expired bridge prevented initial load")
	// Initial/repeated loading of an unpruned Secret must also accept the
	// current path, including a leaf issued after the old bridge expired.
	newLeaf := servingTestCertificate(t, 103, after.Add(time.Hour), after.Add(7*24*time.Hour), &current, false)
	newLeaf.Certificate = [][]byte{newLeaf.Certificate[0], bridgeDER, old.Certificate[0]}
	writeServingTestPair(t, dir, newLeaf)

	require.NoError(t, r.reloadAt(after.Add(2*time.Hour)))

	handshake(after.Add(2*time.Hour), current.Leaf, true, 1)
	last := r.current.Load()

	for _, scenario := range []string{"future bridge", "wrong EKU", "path length", "malformed suffix", "expired leaf"} {
		t.Run(scenario, func(t *testing.T) {
			bad := newLeaf
			bad.Certificate = append([][]byte(nil), newLeaf.Certificate...)
			template := bridgeTemplate

			switch scenario {
			case "future bridge":
				template.NotBefore, template.NotAfter = after.Add(3*time.Hour), after.Add(4*time.Hour)
			case "wrong EKU":
				template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			case "path length":
				rootTemplate := *old.Leaf
				rootTemplate.MaxPathLen, rootTemplate.MaxPathLenZero = 0, true

				der, err := x509.CreateCertificate(rand.Reader, &rootTemplate, &rootTemplate, old.Leaf.PublicKey, old.PrivateKey)
				require.NoError(t, err)

				bad.Certificate[2] = der
			case "malformed suffix":
				bad.Certificate[2] = []byte{1, 2, 3}
			case "expired leaf":
				bad = leaf
			}

			if scenario == "future bridge" || scenario == "wrong EKU" {
				der, err := x509.CreateCertificate(rand.Reader, &template, old.Leaf, current.Leaf.PublicKey, old.PrivateKey)
				require.NoError(t, err)

				bad.Certificate[1] = der
			}

			writeServingTestPair(t, dir, bad)

			at := after.Add(2 * time.Hour)
			if scenario == "expired leaf" {
				at = leaf.Leaf.NotAfter
			}

			require.Error(t, r.reloadAt(at), "invalid replacement accepted")
			require.Same(t, last, r.current.Load(), "last good lost")
		})
	}
}

func testCachedHandshake(t *testing.T, r *servingCertificateReloader, at time.Time, root *x509.Certificate, want bool, length int) {
	t.Helper()

	roots := x509.NewCertPool()
	roots.AddCert(root)

	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		defer serverSide.Close()

		server := tls.Server(serverSide, &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.getCertificateAt(at) }})
		done <- server.HandshakeContext(ctx)
	}()

	peer := tls.Client(clientSide, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1", Time: func() time.Time { return at }})
	err := peer.HandshakeContext(ctx)
	require.Equal(t, want, err == nil, "handshake at %s: %v", at, err)

	if want {
		require.Len(t, peer.ConnectionState().PeerCertificates, length)
	}

	clientSide.Close()

	err = <-done
	if want {
		require.NoError(t, err)
	}
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string { return "temporary accept failure" }

func (temporaryAcceptError) Timeout() bool { return false }

func (temporaryAcceptError) Temporary() bool { return true }

func TestTransportTemporaryAcceptRecoversTLS(t *testing.T) {
	f := newServingFixture(t)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	first := true
	listener := teardownListener{Listener: raw, close: raw.Close, accept: func() (net.Conn, error) {
		if first {
			first = false
			return nil, fmt.Errorf("accept: %w", temporaryAcceptError{})
		}

		return raw.Accept()
	}}
	l := newTransportListener(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate), Limits{MaxConnections: 1, MaxConcurrentHandshakes: 1, HandshakeTimeout: time.Second, WriteTimeout: 30 * time.Second})

	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}

		select {
		case <-l.acceptDone:
		case <-time.After(time.Second):
			t.Error("accept pump leaked")
		}

		awaitTransport(t, l, 0, 0)
	})

	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()

	client := tls.Client(dialTransport(t, l), &tls.Config{RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	secured, ok := conn.(*tls.Conn)
	if !ok || !secured.ConnectionState().HandshakeComplete || len(secured.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("recovered accept lost TLS or client certificate")
	}

	closeTransport(conn)
	awaitTransport(t, l, 0, 0)
}

func TestTransportTemporaryAcceptBackoffCapResetAndTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		accepted, peer := net.Pipe()
		defer accepted.Close()
		defer peer.Close()

		terminal := errors.New("terminal accept failure")

		var calls []time.Time

		listener := teardownListener{close: func() error { return nil }, accept: func() (net.Conn, error) {
			calls = append(calls, time.Now())
			switch len(calls) {
			case 11:
				return accepted, nil
			case 13:
				return nil, terminal
			default:
				return nil, temporaryAcceptError{}
			}
		}}
		// Zero connection capacity rejects the successful accept without a TLS
		// worker; even rejected sockets must reset the accept-error backoff.
		l := newTransportListener(t.Context(), listener, &tls.Config{}, Limits{})
		defer l.Close()

		<-l.acceptDone

		want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond, 320 * time.Millisecond, 640 * time.Millisecond, time.Second, time.Second, 0, 5 * time.Millisecond}
		if len(calls) != len(want)+1 {
			t.Fatalf("accept calls=%d", len(calls))
		}

		for i, delay := range want {
			if got := calls[i+1].Sub(calls[i]); got != delay {
				t.Fatalf("retry %d delay=%s want=%s", i, got, delay)
			}
		}

		for range 2 {
			if conn, err := l.Accept(); conn != nil || !errors.Is(err, terminal) {
				t.Fatalf("terminal accept=%v, %v", conn, err)
			}
		}

		if len(l.connections) != 0 || len(l.handshakes) != 0 {
			t.Fatal("accept errors leaked admission")
		}
	})
}

func TestTransportTemporaryAcceptCancellation(t *testing.T) {
	for _, closeListener := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeListener), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				calls := 0
				listener := teardownListener{close: func() error { return nil }, accept: func() (net.Conn, error) {
					calls++
					return nil, temporaryAcceptError{}
				}}

				l := newTransportListener(ctx, listener, &tls.Config{}, Limits{})
				defer l.Close()
				// Reach the capped retry window. Virtual time and exact call
				// counts prove the pump sleeps rather than spinning/spawning work.
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()

				require.Equal(t, 9, calls, "accept calls")

				start := time.Now()

				if closeListener {
					require.NoError(t, l.Close())
				} else {
					cancel()
				}

				synctest.Wait()

				select {
				case <-l.acceptDone:
				default:
					t.Fatal("cancellation left accept pump sleeping")
				}

				conn, err := l.Accept()
				require.Nil(t, conn)
				require.ErrorIs(t, err, net.ErrClosed)
				require.Zero(t, time.Since(start))
				require.Equal(t, 9, calls)
				require.Empty(t, l.connections)
				require.Empty(t, l.handshakes)
			})
		})
	}
}

func testTransport(t *testing.T, f *servingFixture, connections, handshakes int, deadline time.Duration) *transportListener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	l := newTransportListenerWithMetrics(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate), Limits{MaxConnections: connections, MaxConcurrentHandshakes: handshakes, HandshakeTimeout: deadline, WriteTimeout: 30 * time.Second}, newTransportMetrics(prometheus.NewPedanticRegistry()))

	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}

		awaitTransport(t, l, 0, 0)
	})

	return l
}

func awaitTransport(t *testing.T, l *transportListener, connections, handshakes int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for len(l.connections) != connections || len(l.handshakes) != handshakes {
		if time.Now().After(deadline) {
			t.Fatalf("transport slots: connections=%d handshakes=%d, want %d/%d", len(l.connections), len(l.handshakes), connections, handshakes)
		}

		time.Sleep(time.Millisecond)
	}
}

func dialTransport(t *testing.T, l *transportListener) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeTransport(conn) })

	return conn
}

func expectTransportClosed(t *testing.T, conn net.Conn) {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	_, err := conn.Read(make([]byte, 1))

	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("excess or closed transport retained: %v", err)
	}
}

func TestTransportSilentPeersBoundAndRelease(t *testing.T) {
	for _, limits := range []struct {
		name                    string
		connections, handshakes int
	}{
		{"connections", 2, 3}, {"handshakes", 3, 2},
	} {
		t.Run(limits.name, func(t *testing.T) {
			f := newServingFixture(t)
			l := testTransport(t, f, limits.connections, limits.handshakes, time.Minute)
			first, second := dialTransport(t, l), dialTransport(t, l)
			awaitTransport(t, l, 2, 2)
			assertTransportMetrics(t, l, 2, 2, 0, 0, 0)
			// Rejection never enters a per-peer waiter or handshake goroutine.
			for range 16 {
				expectTransportClosed(t, dialTransport(t, l))
			}

			awaitTransport(t, l, 2, 2)

			var connectionRejected, handshakeRejected float64
			if limits.name == "connections" {
				connectionRejected = 16
			} else {
				handshakeRejected = 16
			}

			assertTransportMetrics(t, l, 2, 2, connectionRejected, handshakeRejected, 0)
			closeTransport(first)
			awaitTransport(t, l, 1, 1)
			assertTransportMetrics(t, l, 1, 1, connectionRejected, handshakeRejected, 0)
			dialTransport(t, l)
			awaitTransport(t, l, 2, 2)

			if err := l.Close(); err != nil {
				t.Fatal(err)
			}

			expectTransportClosed(t, second)
			awaitTransport(t, l, 0, 0)
			assertTransportMetrics(t, l, 0, 0, connectionRejected, handshakeRejected, 0)
		})
	}
}

func TestTransportHandshakeDeadlineAndFailure(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 1, 1, 100*time.Millisecond)
	conn := dialTransport(t, l)
	awaitTransport(t, l, 1, 1)
	assertTransportMetrics(t, l, 1, 1, 0, 0, 0)
	expectTransportClosed(t, conn)
	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 0, 0, 1)

	conn = dialTransport(t, l)
	if _, err := io.WriteString(conn, "not a TLS record"); err != nil {
		t.Fatal(err)
	}

	expectTransportClosed(t, conn)
	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 0, 0, 1)
}

func TestTransportPartialFlightTimeoutAndRecovery(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 1, 1, 200*time.Millisecond)
	entered, resume := make(chan struct{}), make(chan struct{})

	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()

	client := tls.Client(dialTransport(t, l), &tls.Config{
		RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			close(entered)
			<-resume

			return &f.certificate, nil
		},
	})
	done := make(chan error, 1)

	go func() { done <- client.HandshakeContext(f.ctx) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not receive the server flight")
	}

	awaitTransport(t, l, 1, 1)
	assertTransportMetrics(t, l, 1, 1, 0, 0, 0)
	expectTransportClosed(t, dialTransport(t, l))
	// No final client flight arrives. Both budgets must recover without waiting
	// for the independent 30-second response write budget.
	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 1, 0, 1)
	unblock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("partial client handshake leaked")
	}

	fresh := tls.Client(dialTransport(t, l), &tls.Config{RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13})
	require.NoError(t, fresh.HandshakeContext(f.ctx), "admission did not recover")

	conn, err := l.Accept()
	require.NoError(t, err)
	awaitTransport(t, l, 1, 0)

	var closes sync.WaitGroup
	for range 8 {
		closes.Go(func() { closeTransport(conn) })
	}

	closes.Go(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	closes.Wait()
	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 1, 0, 1)
}

func TestTransportHandshakeBudgetThroughClientCertificate(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 2, 1, time.Minute)

	entered, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)

	raw := dialTransport(t, l)
	conn := tls.Client(raw, &tls.Config{
		RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			close(entered)
			<-resume

			return &f.certificate, nil
		},
	})
	done := make(chan error, 1)

	go func() { done <- conn.HandshakeContext(f.ctx) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("client certificate callback not reached")
	}
	// ServerHello was already delivered: GetConfigForClient has returned, but
	// the server must retain admission while waiting for the client's flight.
	awaitTransport(t, l, 1, 1)
	assertTransportMetrics(t, l, 1, 1, 0, 0, 0)
	expectTransportClosed(t, dialTransport(t, l))
	assertTransportMetrics(t, l, 1, 1, 0, 1, 0)

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 0, 1, 0)
	// The client callback is deliberately parked; resume it during cleanup.
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("client handshake leaked")
		}
	})
}

func TestTransportCompletedTLSRetainsOnlyConnectionSlot(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 1, 1, time.Second)

	client := tls.Client(dialTransport(t, l), &tls.Config{RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err := client.HandshakeContext(f.ctx); err != nil {
		t.Fatal(err)
	}

	awaitTransport(t, l, 1, 0)
	assertTransportMetrics(t, l, 1, 0, 0, 0, 0)

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	secured, ok := conn.(*tls.Conn)
	if !ok || !secured.ConnectionState().HandshakeComplete || len(secured.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("listener lost concrete TLS type or client certificate authentication")
	}

	expectTransportClosed(t, dialTransport(t, l))
	assertTransportMetrics(t, l, 1, 0, 1, 0, 0)
	// Both TLS Close and force-close may happen concurrently; release once.
	closeTransport(conn)
	closeTransport(conn)
	awaitTransport(t, l, 0, 0)
	assertTransportMetrics(t, l, 0, 0, 1, 0, 0)
}

func TestTransportProductionLongPollAndIdleAdmission(t *testing.T) {
	f := newServingFixture(t)
	f.configureServer(func(c *Config) {
		c.Limits.MaxConnections = 2
		c.Limits.MaxConcurrentHandshakes = 1
		c.Limits.HandshakeTimeout = 100 * time.Millisecond
		c.Limits.WriteTimeout = time.Second
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)

	go func() { done <- f.a.Server.serve(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate)) }()

	t.Cleanup(func() {
		f.cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("serve shutdown blocked")
		}
	})
	client := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)

	go func() {
		response, err := client.Get(fmt.Sprintf("https://%s/v1/snapshot?after=%d", listener.Addr(), publication.Sequence()))
		if response != nil {
			response.Body.Close()
		}

		requestDone <- err
	}()

	awaitServerPolls(t, f.a.Server, 1)
	// A long poll outlives the handshake deadline and does not monopolize it.
	time.Sleep(150 * time.Millisecond)

	select {
	case err := <-requestDone:
		t.Fatalf("handshake timeout interrupted an established poll: %v", err)
	default:
	}

	other := f.client(t, &f.certificate)
	response, err := other.Get("https://" + listener.Addr().String() + "/invalid")
	responseBody(t, response, err, http.StatusBadRequest)
	// Both the poll and the HTTP keep-alive consume their connection slots.
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	expectTransportClosed(t, raw)
	f.cancel()

	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("poll retained on shutdown")
	}

	awaitServerPolls(t, f.a.Server, 0)
}

func TestTransportLimitsValidation(t *testing.T) {
	for _, field := range []string{"connections", "handshakes"} {
		for _, value := range []int{0, -1} {
			cfg := testConfig(t).ServerConfig

			if field == "connections" {
				cfg.Limits.MaxConnections = value
			} else {
				cfg.Limits.MaxConcurrentHandshakes = value
			}

			if cfg.Validate() == nil {
				t.Fatalf("accepted %s=%d", field, value)
			}
		}
	}

	for _, value := range []time.Duration{0, -time.Nanosecond, time.Nanosecond, 5 * time.Second} {
		cfg := testConfig(t).ServerConfig

		cfg.Limits.HandshakeTimeout = value
		if value <= 0 {
			require.ErrorIs(t, cfg.Validate(), wire.InvalidRequest)
		} else {
			require.NoError(t, cfg.Validate())
			cfg.Limits.WriteTimeout = 0
			require.ErrorIs(t, cfg.Validate(), wire.InvalidRequest, "handshake timeout must not replace write validation")
		}
	}
}

var _ net.Listener = (*transportListener)(nil)

func TestServingReadinessRequiresConfiguredHostname(t *testing.T) {
	for _, names := range [][]string{nil, {"other-service.racer.svc"}, {"racer-controller.racer.svc"}, {"*.racer.svc"}} {
		t.Run("names="+strings.Join(names, ","), func(t *testing.T) {
			f := newServingFixture(t)
			certificate := servingTestCertificate(t, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), nil, false)
			leaf := *certificate.Leaf
			leaf.DNSNames = names
			// A matching CommonName alone must not bypass SAN validation.
			leaf.Subject.CommonName = f.a.Server.config.ReplicationServerName

			der, err := x509.CreateCertificate(rand.Reader, &leaf, &leaf, leaf.PublicKey, certificate.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}

			certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: certificate.PrivateKey}
			dir := t.TempDir()
			writeServingTestPair(t, dir, certificate)

			r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
			if err != nil {
				t.Fatal(err)
			}

			f.a.Server.servingCertificate.Store(r)

			wantReady := len(names) != 0 && names[0] != "other-service.racer.svc"
			if ready := f.a.Server.Ready(nil) == nil; ready != wantReady {
				t.Fatalf("ready=%v want=%v", ready, wantReady)
			}
			// Reloading a correctly named certificate repairs readiness without
			// restart; mismatched names do not corrupt issuer trust/readiness.
			writeServingTestPair(t, dir, f.serverCertificate)

			if err := r.reload(); err != nil {
				t.Fatal(err)
			}

			if err := f.a.Server.Ready(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLeaderCancellationClosesActiveTLSPoll(t *testing.T) {
	f := newServingFixture(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- f.a.Server.serve(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate)) }()

	c := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)

	go func() {
		response, err := c.Get(fmt.Sprintf("https://%s/v1/snapshot?after=%d", listener.Addr(), publication.Sequence()))
		if response != nil {
			response.Body.Close()
		}

		requestDone <- err
	}()

	awaitServerPolls(t, f.a.Server, 1)
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

	awaitServerPolls(t, f.a.Server, 0)
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

			publication, err := f.a.authority.Current()
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			r, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.Sequence()), nil)
			require.NoError(t, err)

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

			awaitServerPolls(t, f.a.Server, 1)

			if !expiration {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("poll outlived expiration/cancellation")
			}

			awaitServerPolls(t, f.a.Server, 0)
		})
	}
}

func TestBootstrapReadDeadlineAndChunkedBound(t *testing.T) {
	f := newServingFixture(t)
	f.configureServer(func(c *Config) { c.Limits.WriteTimeout = 100 * time.Millisecond })
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
	f.configureServer(func(c *Config) { c.Limits.WriteTimeout = 200 * time.Millisecond })
	// Exercise socket backpressure without constructing a large topology. The
	// immutable publication remains valid JSON with bounded trailing whitespace.
	largeFixturePublication(t, f)
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
		n := f.a.Server.polls.count()

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

func TestTLSNodeExclusionRemovesRoutingMembershipWhilePolling(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	c := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		response, err := c.Get(fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.Sequence()))
		body := responseBody(t, response, err, 200)

		updated, decodeErr := wire.DecodePublication(bytes.NewReader(body))
		if decodeErr != nil || len(updated.Members) != 0 {
			t.Errorf("exclusion must remove routing membership: %v", decodeErr)
		}
	}()

	awaitServerPolls(t, f.a.Server, 1)

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
	case <-time.After(5 * time.Second):
		t.Fatal("routing membership poll did not wake after exclusion")
	}
}
