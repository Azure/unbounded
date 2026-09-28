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
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func servingTestCertificate(t *testing.T, serial int64, before, after time.Time, parent *tls.Certificate, ca bool) tls.Certificate {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fmt.Sprint(serial)}, NotBefore: before, NotAfter: after, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: ca, BasicConstraintsValid: true}
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

func TestServingCertificateProjectionAndRecovery(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := servingTestCertificate(t, 1, now.Add(-time.Hour), now.Add(time.Hour), nil, false)

	second := servingTestCertificate(t, 2, now.Add(-time.Hour), now.Add(time.Hour), nil, false)
	for name, certificate := range map[string]tls.Certificate{"one": first, "two": second} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}

		writeServingTestPair(t, path, certificate)
	}

	project := func(generation string) {
		t.Helper()

		if err := os.Symlink(generation, filepath.Join(dir, "..next")); err != nil {
			t.Fatal(err)
		}

		if err := os.Rename(filepath.Join(dir, "..next"), filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	project("one")

	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	r, err := newServingCertificateReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}

	project("two")

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	if r.current.Load().certificate.Leaf.SerialNumber.Int64() != 2 {
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

	if err := os.Remove(filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("one/tls.key", filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}

	project("two")

	if err := r.reload(); err == nil || r.current.Load() != initial {
		t.Fatal("mixed generation accepted")
	}

	if err := os.Remove(filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("..data/tls.key", filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}

	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	if r.current.Load() == initial {
		t.Fatal("projection did not recover")
	}

	last := r.current.Load()

	if err := os.WriteFile(filepath.Join(dir, "two/tls.crt"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.reload(); err == nil || r.current.Load() != last {
		t.Fatal("malformed update replaced certificate")
	}

	if err := os.Remove(filepath.Join(dir, "two/tls.key")); err != nil {
		t.Fatal(err)
	}

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

	if r.current.Load().certificate.Leaf.SerialNumber.Int64() != 2 {
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
		go r.run(ctx, time.Second)
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
		<-r.done
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
	f.a.Server.Config.TLSCertificateFile = filepath.Join(dir, "tls.crt")

	f.a.Server.Config.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
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
	f.a.Server.Config.TLSCertificateFile = filepath.Join(dir, "tls.crt")
	f.a.Server.Config.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	now := time.Now()
	root := servingTestCertificate(t, 10, now.Add(-time.Hour), now.Add(time.Hour), nil, true)
	first := servingTestCertificate(t, 11, now.Add(-time.Minute), now.Add(time.Hour), &root, false)
	// The replacement uses a new CA cross-signed by the old root. A client
	// retaining only the old root must still complete fresh TLS handshakes.
	newRoot := servingTestCertificate(t, 20, now.Add(-time.Hour), now.Add(time.Hour), nil, true)

	crossDER, err := x509.CreateCertificate(rand.Reader, newRoot.Leaf, root.Leaf, newRoot.Leaf.PublicKey, root.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	second := servingTestCertificate(t, 12, now.Add(-time.Minute), now.Add(time.Hour), &newRoot, false)
	second.Certificate = [][]byte{second.Certificate[0], crossDER, root.Certificate[0]}
	f.roots = x509.NewCertPool()
	f.roots.AddCert(root.Leaf)
	writeServingTestPair(t, dir, first)

	config, err := f.a.Server.TLSConfig(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

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

	if response.TLS.PeerCertificates[0].SerialNumber.Int64() != 11 {
		t.Fatal("wrong initial certificate")
	}

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
	fresh := f.client(t, nil)
	fresh.Transport.(*http.Transport).DisableKeepAlives = true
	deadline := time.Now().Add(5 * time.Second)

	for {
		freshResponse, freshErr := fresh.Get(s.URL + wire.SnapshotPath)
		responseBody(t, freshResponse, freshErr, http.StatusUnauthorized)

		if freshResponse.TLS.PeerCertificates[0].SerialNumber.Int64() == 12 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("new handshakes did not see replacement")
		}

		time.Sleep(10 * time.Millisecond)
	}

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

		if result.response.TLS.PeerCertificates[0].SerialNumber.Int64() != 11 {
			t.Fatal("long poll reconnected")
		}
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

	if !reused || response.TLS.PeerCertificates[0].SerialNumber.Int64() != 11 {
		t.Fatal("persistent connection replaced")
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
	if err != nil {
		t.Fatal(err)
	}

	leaf.Certificate = [][]byte{leaf.Certificate[0], bridgeDER, old.Certificate[0]}
	dir := t.TempDir()
	writeServingTestPair(t, dir, leaf)

	r := &servingCertificateReloader{certificateFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key")}
	if err := r.reloadAt(now); err != nil {
		t.Fatal(err)
	}

	initial := r.current.Load()
	// Remove source files: selection after the frozen expiration boundary must
	// work using only immutable cached, prevalidated chain prefixes.
	if err := os.Remove(r.certificateFile); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(r.keyFile); err != nil {
		t.Fatal(err)
	}

	handshake := func(at time.Time, root *x509.Certificate, want bool, length int) {
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
		if (err == nil) != want {
			t.Fatalf("handshake at %s: %v, want success=%v", at, err, want)
		}

		if want && len(peer.ConnectionState().PeerCertificates) != length {
			t.Fatalf("chain length=%d, want %d", len(peer.ConnectionState().PeerCertificates), length)
		}

		clientSide.Close()

		if err := <-done; want && err != nil {
			t.Fatal(err)
		}
	}
	handshake(now, old.Leaf, true, 3)
	handshake(now, current.Leaf, true, 3)
	after := old.Leaf.NotAfter
	handshake(after, current.Leaf, true, 1)
	handshake(after, old.Leaf, false, 0)
	handshake(leaf.Leaf.NotAfter, current.Leaf, false, 0)

	if r.current.Load() != initial {
		t.Fatal("handshake changed published certificate")
	}
	// The operator normally sends leaf + bridges, omitting the old root.
	// Exercise that wire layout as well as the explicit-root layout above.
	withoutRoot := leaf
	withoutRoot.Certificate = leaf.Certificate[:2:2]
	writeServingTestPair(t, dir, withoutRoot)

	if err := r.reloadAt(now); err != nil {
		t.Fatal(err)
	}

	handshake(now, old.Leaf, true, 2)
	handshake(after, current.Leaf, true, 1)
	handshake(after, old.Leaf, false, 0)

	if err := r.reloadAt(after); err != nil {
		t.Fatal("expired bridge prevented initial load", err)
	}
	// Initial/repeated loading of an unpruned Secret must also accept the
	// current path, including a leaf issued after the old bridge expired.
	newLeaf := servingTestCertificate(t, 103, after.Add(time.Hour), after.Add(7*24*time.Hour), &current, false)
	newLeaf.Certificate = [][]byte{newLeaf.Certificate[0], bridgeDER, old.Certificate[0]}
	writeServingTestPair(t, dir, newLeaf)

	if err := r.reloadAt(after.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}

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
				if err != nil {
					t.Fatal(err)
				}

				bad.Certificate[2] = der
			case "malformed suffix":
				bad.Certificate[2] = []byte{1, 2, 3}
			case "expired leaf":
				bad = leaf
			}

			if scenario == "future bridge" || scenario == "wrong EKU" {
				der, err := x509.CreateCertificate(rand.Reader, &template, old.Leaf, current.Leaf.PublicKey, old.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}

				bad.Certificate[1] = der
			}

			writeServingTestPair(t, dir, bad)

			at := after.Add(2 * time.Hour)
			if scenario == "expired leaf" {
				at = leaf.Leaf.NotAfter
			}

			if err := r.reloadAt(at); err == nil || r.current.Load() != last {
				t.Fatal("invalid replacement accepted or last good lost")
			}
		})
	}
}
