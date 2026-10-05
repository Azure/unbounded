// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

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

func TestKeyringExpiredPreparationRecovery(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmtBool(prepared), func(t *testing.T) {
			r, now := testKeyring(t)
			issuer := testIssuer(r)
			runKeys(t, r)

			_, _, initial, _ := keyState(t, r)
			if prepared {
				*now = initial.NextRotation

				runKeys(t, r)
			}

			*now = now.Add(30 * 24 * time.Hour)
			// An expired active issuer cannot sign, but rotation can recover by
			// staging fresh trust and waiting the complete preparation interval.
			if _, err := issuer.TrustRoots(context.Background()); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("expired issuer accepted: %v", err)
			}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); !errors.Is(err, wire.Unavailable) || trustReady(r.Trust) {
				t.Fatalf("expired active readiness: %v", err)
			}

			_, _, staged, _ := keyState(t, r)
			if !staged.ActivateAt.Equal(now.Add(r.Config.Rotation.PrepareFor)) || staged.ActiveIssuer != initial.ActiveIssuer {
				t.Fatal("recovery skipped preparation")
			}

			*now = staged.ActivateAt

			runKeys(t, r)

			if _, err := issuer.TrustRoots(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func fmtBool(v bool) string {
	if v {
		return "prepared"
	}

	return "active"
}
