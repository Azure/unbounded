// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestPreparedIssuerCoversReplacementActivation(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, margin := range []time.Duration{-time.Second, 0, time.Second} {
			t.Run(fmt.Sprintf("early=%v/margin=%s", early, margin), func(t *testing.T) {
				r, now := testKeyring(t)
				runKeys(t, r)
				_, _, initial, _ := keyState(t, r)
				*now = initial.NextRotation

				runKeys(t, r)
				_, bundle, state, material := keyState(t, r)
				oldID := state.PreparedIssuer

				activation := state.ActivateAt
				if !early {
					// Late activation must schedule the next cycle from actual time.
					*now = state.ActivateAt.Add(2 * time.Hour)
					activation = *now
				}

				short := editSigningCertificate(t, material.Keys[oldID], func(cert *x509.Certificate) {
					cert.NotAfter = activation.Add(r.Config.Rotation.Interval + r.Config.CertificateLifetime + margin)
				})
				shortID := rootID(short.Certificate)

				delete(material.Keys, oldID)
				material.Keys[shortID] = short
				state.PreparedIssuer = shortID

				for i, root := range bundle.PeerTrustRoots {
					if rootID(root) == oldID {
						bundle.PeerTrustRoots[i] = short.Certificate
					}
				}

				bundle.Generation++
				writeSigningCredentials(t, r, bundle, state, material)
				runKeys(t, r)

				_, after, next, keys := keyState(t, r)
				if margin < 0 {
					if containsRoot(after, shortID) || next.PreparedIssuer == shortID || next.ActiveIssuer != initial.ActiveIssuer || !next.ActivateAt.Equal(now.Add(r.Config.Rotation.PrepareFor)) {
						t.Fatal("insufficient signing horizon did not restart preparation")
					}

					if _, retained := keys.Keys[shortID]; retained {
						t.Fatal("stale private material retained")
					}
				} else if early {
					if next.PreparedIssuer != shortID || !next.ActivateAt.Equal(state.ActivateAt) || after.Generation != bundle.Generation {
						t.Fatal("usable preparation changed before activation")
					}
				} else if next.ActiveIssuer != shortID || next.PreparedIssuer != "" {
					t.Fatal("usable prepared issuer did not activate")
				}

				if next.PreparedIssuer != "" {
					*now = next.ActivateAt

					runKeys(t, r)
				}

				_, _, active, _ := keyState(t, r)
				*now = active.NextRotation

				runKeys(t, r)
				_, _, replacement, _ := keyState(t, r)
				*now = replacement.ActivateAt.Add(-time.Second)

				identity, request, _ := issuanceRequest(t, r)
				if _, err := testIssuer(r).Issue(t.Context(), identity, request); err != nil {
					t.Fatalf("issuer failed before replacement activation: %v", err)
				}

				*now = replacement.ActivateAt

				runKeys(t, r)
			})
		}
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
