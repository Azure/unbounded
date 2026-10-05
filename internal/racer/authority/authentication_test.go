// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func testIssuer(r *KeyringReconciler) *Issuer {
	return &Issuer{APIReader: r.APIReader, Config: r.Config, Trust: r.Trust, CatalogGate: r.CatalogGate, Now: r.Now}
}

// TrustRoots is a test adapter for authoritative signing observations. Production
// serving uses local Trust; only issuance and reconciliation read durable roots.
func (i *Issuer) TrustRoots(ctx context.Context) (*x509.CertPool, error) {
	state, err := i.loadSigning(ctx, i.now())
	if err != nil {
		return nil, err
	}

	return state.roots, nil
}

func issuanceRequest(t *testing.T, r *KeyringReconciler) (NodeIdentity, wire.BootstrapRequest, ed25519.PublicKey) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "untrusted"}, DNSNames: []string{"attacker"}, URIs: []*url.URL{{Scheme: "spiffe", Host: "attacker", Path: "/node/attacker"}}}, key)
	if err != nil {
		t.Fatal(err)
	}

	return NodeIdentity{cluster: r.Config.Cluster, node: wire.NodeID(testNodeUID), expires: r.now().Add(time.Hour)}, wire.BootstrapRequest{SchemaVersion: wire.SchemaVersion, Cluster: r.Config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares}, pub
}

func decodeIssuedResponse(t *testing.T, encoded []byte) wire.BootstrapResponse {
	t.Helper()

	if len(encoded) == 0 || len(encoded) > wire.MaxBootstrapBytes {
		t.Fatalf("issued response size: %d", len(encoded))
	}

	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func TestIssuerCertificateContractAndTrustRotation(t *testing.T) {
	r, now := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, pub := issuanceRequest(t, r)

	encoded, err := issuer.Issue(context.Background(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	response := decodeIssuedResponse(t, encoded)

	cert, err := x509.ParseCertificate(response.CertificateChain[0])
	if err != nil {
		t.Fatal(err)
	}

	if response.Node != identity.Node() || response.Cluster != request.Cluster || response.Enrollment != request.Enrollment || len(response.CertificateChain) != 2 {
		t.Fatal("response correlation")
	}

	if cert.IsCA || cert.Subject.CommonName != "" || len(cert.DNSNames) != 0 || len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://"+string(identity.Cluster())+"/node/"+string(identity.Node()) || cert.KeyUsage != x509.KeyUsageDigitalSignature || !cert.NotAfter.Equal(now.Add(wire.CertificateLifetime)) || !cert.NotBefore.Equal(now.Add(-certificateClockSkew)) || !bytes.Equal(cert.PublicKey.(ed25519.PublicKey), pub) {
		t.Fatal("certificate identity or policy")
	}

	roots, err := issuer.TrustRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}

	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("node can act as HTTPS server")
	}

	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)

	identity.expires = now.Add(time.Hour)

	encoded, err = issuer.Issue(context.Background(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	staged := decodeIssuedResponse(t, encoded)

	if !bytes.Equal(staged.CertificateChain[1], response.CertificateChain[1]) {
		t.Fatal("prepared issuer signed early")
	}

	_, _, preparation, _ := keyState(t, r)
	*now = preparation.ActivateAt

	runKeys(t, r)

	identity.expires = now.Add(time.Hour)

	encoded, err = issuer.Issue(context.Background(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	active := decodeIssuedResponse(t, encoded)

	if bytes.Equal(active.CertificateChain[1], response.CertificateChain[1]) {
		t.Fatal("new issuer not activated")
	}

	roots, err = issuer.TrustRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	oldLeaf, err := x509.ParseCertificate(staged.CertificateChain[0])
	if err != nil {
		t.Fatal(err)
	}

	if _, err := oldLeaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("old leaf lost overlap: %v", err)
	}

	*now = now.Add(r.Config.Rotation.RetainFor)
	runKeys(t, r)

	roots, err = issuer.TrustRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Use a time at which the old leaf was valid to isolate root removal.
	if _, err := oldLeaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: oldLeaf.NotBefore.Add(time.Minute), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("retired root remains trusted")
	}
}

func TestIssuerRejectsUntrustedRequests(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)

	identity, request, _ := issuanceRequest(t, r)
	for _, scenario := range []string{"zero identity", "expired identity", "wrong cluster", "unsupported version", "bad enrollment", "malformed csr", "bad proof", "wrong algorithm", "oversized", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			id, req := identity, request

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			switch scenario {
			case "zero identity":
				id = NodeIdentity{}
			case "expired identity":
				id.expires = r.now()
			case "wrong cluster":
				req.Cluster = wire.ClusterID(testNodeUID)
			case "unsupported version":
				req.SchemaVersion++
			case "bad enrollment":
				req.Enrollment = "bad"
			case "malformed csr":
				req.CSRDER = []byte("invalid DER")
			case "bad proof":
				req.CSRDER = bytes.Clone(req.CSRDER)
				req.CSRDER[len(req.CSRDER)-1] ^= 1
			case "wrong algorithm":
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}

				req.CSRDER, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
				if err != nil {
					t.Fatal(err)
				}
			case "oversized":
				req.CSRDER = make([]byte, wire.MaxBootstrapBytes+1)
			case "canceled":
				cancel()
			}

			response, err := issuer.Issue(ctx, id, req)
			if err == nil || response != nil {
				t.Fatal("untrusted issuance accepted")
			}
		})
	}
}

func TestIssuerShortLifetimeAndRetirement(t *testing.T) {
	r, now := testKeyring(t)
	r.Config.CertificateLifetime = 2 * time.Minute
	r.Config.Rotation = RotationPolicy{5 * time.Minute, 20 * time.Second, 2 * time.Minute}
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)

	encoded, err := issuer.Issue(context.Background(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	response := decodeIssuedResponse(t, encoded)

	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	if err != nil || !leaf.NotAfter.Equal(now.Add(2*time.Minute)) || !leaf.NotBefore.Equal(now.Add(-certificateClockSkew)) {
		t.Fatalf("short leaf lifetime: %v", err)
	}

	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)
	_, _, prepared, _ := keyState(t, r)
	*now = prepared.ActivateAt

	runKeys(t, r)

	encoded, err = issuer.Issue(context.Background(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	renewed := decodeIssuedResponse(t, encoded)
	if bytes.Equal(response.CertificateChain[1], renewed.CertificateChain[1]) {
		t.Fatalf("short rotation issuer activation: %v", err)
	}

	*now = now.Add(2 * time.Minute)

	runKeys(t, r)

	_, bundle, _, material := keyState(t, r)
	if containsRoot(bundle, initial.ActiveIssuer) || len(material.Keys) != 1 {
		t.Fatal("short rotation did not retire old public/private issuer")
	}
}

func TestIssuerFullEncodedRequestBound(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	for _, size := range []int{47 * 1024, 49 * 1024} {
		request.CSRDER, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: strings.Repeat("x", size)}}, key)
		if err != nil {
			t.Fatal(err)
		}

		if len(request.CSRDER) >= wire.MaxBootstrapBytes {
			t.Fatal("fixture must fit the raw DER bound")
		}

		encoded, err := issuer.Issue(context.Background(), identity, request)
		if size == 49*1024 {
			if !errors.Is(err, wire.TooLarge) || encoded != nil {
				t.Fatalf("encoded request overflow: %v, %d response bytes", err, len(encoded))
			}

			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		response := decodeIssuedResponse(t, encoded)
		if response.Enrollment != request.Enrollment || response.Node != identity.Node() {
			t.Fatal("large valid request lost correlation")
		}
	}
}

func TestIssuerConcurrentIssuanceAndReconciliation(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 4 {
				if _, err := issuer.Issue(context.Background(), identity, request); err != nil {
					t.Error(err)
				}

				if _, err := issuer.TrustRoots(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}

	for range 4 {
		runKeys(t, r)
	}

	wg.Wait()
}

func writeSigningCredentials(t *testing.T, r *KeyringReconciler, b wire.KeyringBundle, s RotationState, m issuerMaterial) {
	t.Helper()

	bundle, err := wire.EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}

	rotation, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}

	material, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string]map[string][]byte{
		r.Config.CredentialsSecretName: {"issuer.json": material, "bundle.json": bundle, "rotation.json": rotation},
	} {
		secret := &corev1.Secret{}
		if err := r.APIReader.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, secret); err != nil {
			t.Fatal(err)
		}

		secret.Data = data
		if err := r.Update(t.Context(), secret); err != nil {
			t.Fatal(err)
		}
	}
}

func editSigningCertificate(t *testing.T, m signingMaterial, edit func(*x509.Certificate)) signingMaterial {
	t.Helper()

	cert, key, err := parseSigning(m)
	if err != nil {
		t.Fatal(err)
	}

	edit(cert)

	m.Certificate, err = x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func TestSigningRejectsCorruptPrivateEntries(t *testing.T) {
	for _, role := range []string{"active", "prepared", "retiring", "extra", "pending"} {
		for _, corruption := range []string{"missing", "root binding", "certificate", "trailing certificate bytes", "private key", "key mismatch", "not CA", "constraints", "key usage", "self signature"} {
			t.Run(role+"/"+corruption, func(t *testing.T) {
				r, now := testKeyring(t)
				runKeys(t, r)
				_, b, s, m := keyState(t, r)
				id := s.ActiveIssuer

				if role != "active" {
					cert, key, err := generateIssuer(*now, r.Config)
					if err != nil {
						t.Fatal(err)
					}

					id = rootID(cert)
					m.Keys[id] = signingMaterial{Certificate: cert, PrivateKey: key}
				}

				switch role {
				case "prepared":
					b.PeerTrustRoots = append(b.PeerTrustRoots, m.Keys[id].Certificate)
					s.PreparedIssuer = id
					s.ActivateAt = s.NextRotation.Add(r.Config.Rotation.PrepareFor)
				case "retiring":
					b.PeerTrustRoots = append(b.PeerTrustRoots, m.Keys[id].Certificate)
					s.Retiring[id] = s.NextRotation.Add(time.Hour)
				case "extra", "pending":
					// Former unpublished roles are now rejected even when well formed.
					writeSigningCredentials(t, r, b, s, m)

					if _, err := loadSigning(t.Context(), r.APIReader, r.Config, *now); !errors.Is(err, wire.Unavailable) {
						t.Fatalf("unpublished private material accepted: %v", err)
					}
					// Publish the root to exercise every corruption below independently.
					b.PeerTrustRoots = append(b.PeerTrustRoots, m.Keys[id].Certificate)
					s.Retiring[id] = s.NextRotation.Add(time.Hour)
				}

				writeSigningCredentials(t, r, b, s, m)

				if _, err := loadSigning(t.Context(), r.APIReader, r.Config, *now); err != nil {
					t.Fatalf("valid %s rejected: %v", role, err)
				}

				bad := m.Keys[id]

				switch corruption {
				case "missing":
					// Every private entry has a published root requiring its presence.
				case "root binding":
					m.Keys["wrong fingerprint"] = bad
				case "certificate":
					bad.Certificate = []byte("invalid DER")
				case "trailing certificate bytes":
					bad.Certificate = append(bytes.Clone(bad.Certificate), 0)
				case "private key":
					bad.PrivateKey = []byte("invalid PKCS8")
				case "key mismatch":
					_, key, err := generateIssuer(*now, r.Config)
					if err != nil {
						t.Fatal(err)
					}

					bad.PrivateKey = key
				case "not CA":
					bad = editSigningCertificate(t, bad, func(c *x509.Certificate) {
						c.IsCA = false
						c.MaxPathLenZero = false
						c.MaxPathLen = -1
					})
				case "constraints":
					bad = editSigningCertificate(t, bad, func(c *x509.Certificate) { c.BasicConstraintsValid = false })
				case "key usage":
					bad = editSigningCertificate(t, bad, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature })
				case "self signature":
					bad.Certificate = bytes.Clone(bad.Certificate)
					bad.Certificate[len(bad.Certificate)-1] ^= 1
				}

				delete(m.Keys, id)

				if corruption != "missing" && corruption != "root binding" {
					// Rebind modified DER so certificate validation, rather than the
					// fingerprint check, must reject corrupt and non-CA material.
					nextID := rootID(bad.Certificate)
					m.Keys[nextID] = bad

					for i, root := range b.PeerTrustRoots {
						if rootID(root) == id && corruption != "certificate" && corruption != "trailing certificate bytes" {
							b.PeerTrustRoots[i] = bad.Certificate
						}
					}

					if s.ActiveIssuer == id {
						s.ActiveIssuer = nextID
					}

					if s.PreparedIssuer == id {
						s.PreparedIssuer = nextID
					}

					if at, ok := s.Retiring[id]; ok {
						delete(s.Retiring, id)
						s.Retiring[nextID] = at
					}
				}

				writeSigningCredentials(t, r, b, s, m)

				if state, err := loadSigning(t.Context(), r.APIReader, r.Config, *now); !errors.Is(err, wire.Unavailable) || state.certificate != nil || state.key != nil || state.roots != nil {
					t.Fatalf("corrupt credentials exposed signing state: %v", err)
				}

				if _, err := testIssuer(r).TrustRoots(t.Context()); !errors.Is(err, wire.Unavailable) {
					t.Fatalf("issuance accepted corrupt credentials: %v", err)
				}

				if _, err := r.Trust.pool(); err == nil {
					t.Fatal("observed corruption retained trust")
				}

				if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.Unavailable) {
					t.Fatalf("reconciliation accepted corrupt credentials: %v", err)
				}
			})
		}
	}
}

func TestSigningActiveLifetimeBoundary(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, _, s, m := keyState(t, r)

	cert, _, err := parseSigning(m.Keys[s.ActiveIssuer])
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"not yet valid", cert.NotBefore.Add(-time.Second), false},
		{"starts now", cert.NotBefore, true},
		{"full leaf lifetime", cert.NotAfter.Add(-r.Config.CertificateLifetime), true},
		{"short by one second", cert.NotAfter.Add(-r.Config.CertificateLifetime).Add(time.Second), false},
		{"expired", cert.NotAfter, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := loadSigning(t.Context(), r.APIReader, r.Config, tc.at)
			if tc.valid {
				if err != nil || !bytes.Equal(state.certificate.Raw, cert.Raw) || !state.certificate.PublicKey.(ed25519.PublicKey).Equal(state.key.Public()) {
					t.Fatalf("valid active signing state: %v", err)
				}
			} else if !errors.Is(err, wire.Unavailable) {
				t.Fatalf("invalid active lifetime accepted: %v", err)
			}
		})
	}
}

func TestSigningPoolOnlyIncludesTimeValidPublishedRoots(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, b, s, m := keyState(t, r)

	active, _, err := parseSigning(m.Keys[s.ActiveIssuer])
	if err != nil {
		t.Fatal(err)
	}

	want := x509.NewCertPool()
	want.AddCert(active)

	for _, role := range []string{"starts now", "expires now", "future", "extra", "pending"} {
		cert, key, err := generateIssuer(*now, r.Config)
		if err != nil {
			t.Fatal(err)
		}

		material := editSigningCertificate(t, signingMaterial{Certificate: cert, PrivateKey: key}, func(c *x509.Certificate) {
			switch role {
			case "starts now":
				c.NotBefore = *now
			case "expires now":
				c.NotAfter = *now
			case "future":
				c.NotBefore = now.Add(time.Second)
			}
		})
		id := rootID(material.Certificate)
		m.Keys[id] = material

		if role == "extra" || role == "pending" {
			writeSigningCredentials(t, r, b, s, m)

			if _, err := loadSigning(t.Context(), r.APIReader, r.Config, *now); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("unpublished private material accepted: %v", err)
			}

			delete(m.Keys, id)

			continue
		}

		b.PeerTrustRoots = append(b.PeerTrustRoots, material.Certificate)
		s.Retiring[id] = s.NextRotation.Add(time.Hour)

		if role == "starts now" {
			root, _, err := parseSigning(material)
			if err != nil {
				t.Fatal(err)
			}

			want.AddCert(root)
		}
	}

	writeSigningCredentials(t, r, b, s, m)

	state, err := loadSigning(t.Context(), r.APIReader, r.Config, *now)
	if err != nil || !state.roots.Equal(want) || !bytes.Equal(state.certificate.Raw, active.Raw) {
		t.Fatalf("pool or active selection did not respect published authority and time: %v", err)
	}

	for id := range s.Retiring {
		s.Retiring[id] = time.Time{}
		break
	}

	writeSigningCredentials(t, r, b, s, m)

	if _, err := loadSigning(t.Context(), r.APIReader, r.Config, *now); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("zero retirement deadline accepted: %v", err)
	}
}

func TestLeafClockSkewPreservesExpirationAndUsage(t *testing.T) {
	r, now := testKeyring(t)
	// Simulate an issuing leader ahead of a follower's clock.
	*now = now.Add(30 * time.Second)

	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)

	encoded, err := testIssuer(r).Issue(t.Context(), identity, request)
	if err != nil {
		t.Fatal(err)
	}

	response := decodeIssuedResponse(t, encoded)

	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	if err != nil {
		t.Fatal(err)
	}

	root, err := x509.ParseCertificate(response.CertificateChain[1])
	if err != nil {
		t.Fatal(err)
	}

	if !leaf.NotBefore.Equal(now.Add(-time.Minute)) || !leaf.NotAfter.Equal(now.Add(r.Config.CertificateLifetime)) {
		t.Fatal("skew changed forward expiration or validity start")
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)

	for _, tc := range []struct {
		name  string
		at    time.Time
		usage x509.ExtKeyUsage
		valid bool
	}{
		{"follower behind", now.Add(-30 * time.Second), x509.ExtKeyUsageClientAuth, true},
		{"skew boundary", now.Add(-time.Minute), x509.ExtKeyUsageClientAuth, true},
		{"excess skew", now.Add(-time.Minute - time.Second), x509.ExtKeyUsageClientAuth, false},
		{"before expiry", leaf.NotAfter.Add(-time.Second), x509.ExtKeyUsageClientAuth, true},
		{"expired", leaf.NotAfter.Add(time.Second), x509.ExtKeyUsageClientAuth, false},
		{"server usage", *now, x509.ExtKeyUsageServerAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: tc.at, KeyUsages: []x509.ExtKeyUsage{tc.usage}})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}

	state := &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf, root}, VerifiedChains: [][]*x509.Certificate{{leaf, root}}}
	if _, err := AuthenticateCertificate(t.Context(), r.Trust, r.Config, state); err != nil {
		t.Fatalf("follower rejected skewed leader's certificate: %v", err)
	}
	// Expired leaves still fail each authorization, even on a verified connection.
	leaf.NotAfter = time.Now().Add(-time.Second)

	if _, err := AuthenticateCertificate(t.Context(), r.Trust, r.Config, state); !errors.Is(err, wire.Unauthenticated) {
		t.Fatalf("expired certificate accepted: %v", err)
	}
	// Advancing past signing capacity still fails closed, rather than clipping expiry.
	*now = root.NotAfter.Add(-r.Config.CertificateLifetime + time.Second)

	identity.expires = now.Add(time.Hour)
	if _, err := testIssuer(r).Issue(t.Context(), identity, request); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("insufficient issuer lifetime accepted: %v", err)
	}
}
