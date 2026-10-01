// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

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
		{"full leaf lifetime", cert.NotAfter.Add(-r.Config.certificateLifetime()), true},
		{"short by one second", cert.NotAfter.Add(-r.Config.certificateLifetime()).Add(time.Second), false},
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
