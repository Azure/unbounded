// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestKeyringReplayProtectionSurvivesInvalidation(t *testing.T) {
	for _, scenario := range []string{"rollback", "conflicting generation"} {
		for _, restore := range []string{"accepted generation", "new generation"} {
			t.Run(scenario+"/"+restore, func(t *testing.T) {
				r, _ := testKeyring(t)
				runKeys(t, r)
				shared, bundle, _, _ := keyState(t, r)
				writeBundle := func(candidate wire.KeyringBundle) {
					t.Helper()

					encoded, err := wire.EncodeBundle(candidate)
					if err != nil {
						t.Fatal(err)
					}

					if err := r.Get(t.Context(), client.ObjectKeyFromObject(shared), shared); err != nil {
						t.Fatal(err)
					}

					shared.Data["bundle.json"] = encoded
					if err := r.Update(t.Context(), shared); err != nil {
						t.Fatal(err)
					}
				}
				bundle.Generation = 2
				writeBundle(bundle)
				runKeys(t, r)

				accepted, _, err := r.Trust.keyring()
				if err != nil || accepted.generation != 2 {
					t.Fatalf("initial accepted state: %v", err)
				}

				digest := sha256.Sum256([]byte(accepted.encoded))

				candidate, err := wire.DecodeBundle(bytes.NewBufferString(accepted.encoded))
				if err != nil {
					t.Fatal(err)
				}

				if scenario == "rollback" {
					candidate.Generation--
				} else {
					key := candidate.CacheKeys[0]

					candidate.CacheKeys[0], err = wire.NewCacheKey(key.Key, key.State, [32]byte{1})
					if err != nil {
						t.Fatal(err)
					}
				}

				writeBundle(candidate)

				for range 3 {
					_, err := r.Reconcile(t.Context(), ctrl.Request{})
					if !errors.Is(err, wire.Conflict) {
						t.Fatalf("replay was not rejected: %v", err)
					}

					if _, _, err := r.Trust.keyring(); !errors.Is(err, wire.Unavailable) {
						t.Fatalf("replay restored delivery: %v", err)
					}

					if _, err := r.Trust.pool(); !errors.Is(err, wire.Unavailable) || r.Lifecycle.issuer {
						t.Fatalf("replay restored trust: %v", err)
					}

					if r.Trust.highWater != 2 || r.Trust.digest != digest {
						t.Fatal("invalidation lost accepted replay protection")
					}
				}

				if restore == "new generation" {
					bundle = candidate
					bundle.Generation = 3
				}

				writeBundle(bundle)
				runKeys(t, r)

				current, _, err := r.Trust.keyring()
				if err != nil || current.generation != bundle.Generation {
					t.Fatalf("valid restoration failed: %v", err)
				}

				expected, err := wire.EncodeBundle(bundle)
				if err != nil || current.encoded != string(expected) {
					t.Fatalf("restored wrong content: %v", err)
				}

				if _, err := r.Trust.pool(); err != nil || !r.Lifecycle.issuer {
					t.Fatalf("restoration did not restore trust: %v", err)
				}

				if r.Trust.highWater != bundle.Generation || r.Trust.digest != sha256.Sum256(expected) {
					t.Fatal("restoration did not advance replay protection")
				}
			})
		}
	}
}

func TestAcceptedKeyringImmutableAndValidated(t *testing.T) {
	f := newServingFixture(t)
	trust := f.a.Server.Trust

	initial, _, err := trust.keyring()
	if err != nil {
		t.Fatal(err)
	}

	roots, err := trust.pool()
	if err != nil {
		t.Fatal(err)
	}

	_, bundle, _, _ := keyState(t, f.a.Keyring)

	bundle.Generation++
	if err := trust.install(f.ctx, roots, bundle); err != nil {
		t.Fatal(err)
	}

	accepted, _, err := trust.keyring()
	if err != nil {
		t.Fatal(err)
	}

	expected := accepted.encoded
	bundle.PeerTrustRoots[0][0] ^= 0xff

	if accepted.encoded != expected || initial.generation != 1 {
		t.Fatal("caller mutated immutable delivery")
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		if fmt.Sprintf(format, accepted) != "<redacted keyring>" {
			t.Fatal("diagnostic exposed keyring")
		}
	}

	for _, scenario := range []string{"invalid", "rollback", "same generation changed", "canceled", "nil roots"} {
		t.Run(scenario, func(t *testing.T) {
			_, candidate, _, _ := keyState(t, f.a.Keyring)
			candidate.Generation = 3

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			pool := roots

			switch scenario {
			case "invalid":
				candidate.SchemaVersion = 0
			case "rollback":
				candidate.Generation = 1
			case "same generation changed":
				candidate.Generation = 2
				candidate.PeerTrustRoots[0][0] ^= 0xff
			case "canceled":
				cancel()
			case "nil roots":
				pool = nil
			}

			if err := trust.install(ctx, pool, candidate); err == nil {
				t.Fatal("invalid installation succeeded")
			}

			current, _, err := trust.keyring()
			if err != nil || current != accepted {
				t.Fatal("failed installation changed delivery")
			}
		})
	}

	older := wire.Generation(1)

	current, err := trust.waitKeyring(f.ctx, &older)
	if err != nil || current != accepted {
		t.Fatalf("older cursor did not return newest: %v", err)
	}
}

func TestIssuerObservationInvalidatesKeyringDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)

		accepted, _, err := f.a.Server.Trust.keyring()
		if err != nil {
			t.Fatal(err)
		}

		issuer := f.a.Server.Bootstrap.Issuer
		live := issuer.APIReader

		issuer.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("offline")
		}})
		if _, err := issuer.TrustRoots(f.ctx); err == nil {
			t.Fatal("outage hidden")
		}

		if current, _, err := f.a.Server.Trust.keyring(); err != nil || current != accepted {
			t.Fatal("issuer read outage withdrew accepted delivery")
		}

		issuer.APIReader = live
		done := make(chan error, 1)

		go func() { _, err := f.a.Server.Trust.waitKeyring(f.ctx, &accepted.generation); done <- err }()

		synctest.Wait()

		shared, _, _, _ := keyState(t, f.a.Keyring)
		valid := bytes.Clone(shared.Data["bundle.json"])

		shared.Data["bundle.json"] = []byte(`{}`)
		if err := f.a.Topology.Update(f.ctx, shared); err != nil {
			t.Fatal(err)
		}

		if _, err := issuer.TrustRoots(f.ctx); err == nil {
			t.Fatal("invalid authority accepted")
		}

		select {
		case err := <-done:
			if !errors.Is(err, wire.Unavailable) {
				t.Fatalf("invalidated delivery: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("issuer invalidation did not wake poll")
		}

		shared.Data["bundle.json"] = valid
		if err := f.a.Topology.Update(f.ctx, shared); err != nil {
			t.Fatal(err)
		}

		if _, err := issuer.TrustRoots(f.ctx); err != nil {
			t.Fatal(err)
		}

		if _, _, err := f.a.Server.Trust.keyring(); err == nil {
			t.Fatal("issuance restored delivery without reconciliation")
		}

		runKeys(t, f.a.Keyring)

		if _, _, err := f.a.Server.Trust.keyring(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestKeyringReauthenticationCannotDiscloseWithdrawnBundle(t *testing.T) {
	f := newServingFixture(t)
	reads := 0
	f.a.Server.Bootstrap.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			reads++
			if reads == 2 {
				f.a.Server.Trust.invalidate()
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
