// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestCredentialsSingleCreateAndPermanentClaim(t *testing.T) {
	r, _ := testKeyring(t)
	base := r.Client.(client.WithWatch)
	creates := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		creates++

		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Name != r.Config.CredentialsSecretName || len(secret.Data) != 3 {
			t.Fatal("initialization did not create one complete credentials Secret")
		}

		version, _, err := readVersion(ctx, base, r.Config)
		if err != nil {
			t.Fatal(err)
		}

		var material issuerMaterial
		if err := json.Unmarshal(secret.Data["issuer.json"], &material); err != nil {
			t.Fatal(err)
		}

		for id := range material.Keys {
			want := secret.Name + "/" + id
			if version.Annotations[credentialClaim] != want || secret.Annotations[credentialClaim] != want {
				t.Fatal("permanent secretName/initialRootFingerprint claim not durable before Create")
			}
		}

		return c.Create(ctx, obj, opts...)
	}})
	runKeys(t, r)
	runKeys(t, r)

	if creates != 1 {
		t.Fatalf("credential Creates = %d, want 1", creates)
	}
}

func TestCredentialsClaimValidation(t *testing.T) {
	r, _ := testKeyring(t)
	name := r.Config.CredentialsSecretName

	fingerprint := strings.Repeat("ab", 32)
	for _, claim := range []string{"", name + "/", name + "/" + fingerprint + "/extra", "other/" + fingerprint, name + "/" + strings.Repeat("x", 64), name + "/" + strings.ToUpper(fingerprint)} {
		t.Run(claim, func(t *testing.T) {
			if validCredentialClaim(r.Config, claim) {
				t.Fatal("invalid claim accepted")
			}
		})
	}

	if !validCredentialClaim(r.Config, name+"/"+fingerprint) {
		t.Fatal("valid claim rejected")
	}
}

func TestCredentialsMissingOrLegacyEntriesNeverRegenerate(t *testing.T) {
	for _, corruption := range []string{"issuer.json", "bundle.json", "rotation.json", "pending", "trailing metadata", "symmetric retirement", "duplicate material"} {
		t.Run(corruption, func(t *testing.T) {
			r, _ := testKeyring(t)
			runKeys(t, r)
			secret, bundle, state, material := keyState(t, r)

			switch corruption {
			case "duplicate material":
				var document map[string]any
				if err := json.Unmarshal(secret.Data["bundle.json"], &document); err != nil {
					t.Fatal(err)
				}

				keys := document["cache_keys"].([]any)
				keys[1].(map[string]any)["material"] = keys[0].(map[string]any)["material"]
				document["generation"] = fmt.Sprint(uint64(bundle.Generation + 1))
				secret.Data["bundle.json"], _ = json.Marshal(document)
			case "pending":
				secret.Data["issuer.json"], _ = json.Marshal(map[string]any{"keys": material.Keys, "pending": state.ActiveIssuer})
			case "trailing metadata":
				secret.Data["rotation.json"] = append(secret.Data["rotation.json"], []byte(" {}")...)
			case "symmetric retirement":
				state.Retiring[keyID(bundle.CacheKeys[0])] = state.NextRotation
				secret.Data["rotation.json"], _ = json.Marshal(state)
			default:
				delete(secret.Data, corruption)
			}

			if err := r.Update(t.Context(), secret); err != nil {
				t.Fatal(err)
			}

			base := r.Client.(client.WithWatch)

			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("claimed corrupt credentials regenerated")
					return nil
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Fatal("claimed corrupt credentials rewritten")
					return nil
				},
			})
			if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err == nil || trustReady(r.Trust) {
				t.Fatal("corrupt atomic version accepted")
			} else if corruption == "duplicate material" && !errors.Is(err, wire.Unavailable) {
				t.Fatalf("committed duplicate material should be unavailable: %v", err)
			}
		})
	}
}

func TestCredentialsIdlePreservesEncoding(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	secret, _, _, _ := keyState(t, r)

	for _, entry := range []string{"issuer.json", "rotation.json"} {
		var indented bytes.Buffer
		if err := json.Indent(&indented, secret.Data[entry], "", "  "); err != nil {
			t.Fatal(err)
		}

		secret.Data[entry] = indented.Bytes()
	}

	if err := r.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		t.Fatal("idle plan normalized metadata with a write")
		return nil
	}})
	runKeys(t, r)

	after, _, _, _ := keyState(t, r)
	if after.ResourceVersion != secret.ResourceVersion || !reflect.DeepEqual(after.Data, secret.Data) {
		t.Fatal("idle credentials changed")
	}
}

func TestCredentialsCandidateIsOneCoherentCAS(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation
	base := r.Client.(client.WithWatch)
	writes := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		writes++
		secret := obj.(*corev1.Secret)

		bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
		if err != nil {
			t.Fatal(err)
		}

		var (
			rotation RotationState
			material issuerMaterial
		)

		if json.Unmarshal(secret.Data["rotation.json"], &rotation) != nil || json.Unmarshal(secret.Data["issuer.json"], &material) != nil {
			t.Fatal("unreadable candidate metadata")
		}

		candidate := credentialState{bundle: bundle, rotation: rotation, material: material}
		if err := candidate.validateRotation(); err != nil {
			t.Fatal("incoherent candidate")
		}

		if bundle.Generation != 2 || rotation.PreparedIssuer == "" || len(material.Keys) != 2 {
			t.Fatal("candidate is not the complete preparation")
		}

		for _, key := range bundle.CacheKeys {
			if binary.BigEndian.Uint64(key.Key.ID[4:12]) > uint64(bundle.Generation) {
				t.Fatal("creation generation exceeds publication")
			}
		}

		return c.Update(ctx, obj, opts...)
	}})
	runKeys(t, r)

	if writes != 1 {
		t.Fatalf("rotation CAS count = %d", writes)
	}

	accepted, _, err := r.Trust.keyring()
	if err != nil || accepted.generation != 2 {
		t.Fatal("committed publication not installed")
	}
	// The claim never follows rotating issuer identities.
	version, _, err := readVersion(t.Context(), base, r.Config)
	if err != nil || version.Annotations[credentialClaim] != r.Config.CredentialsSecretName+"/"+initial.ActiveIssuer {
		t.Fatal("rotation changed the permanent creation claim")
	}

	if _, err := loadSigning(t.Context(), base, r.Config, *now); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsStalePreparationReplacementIsAtomic(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			runKeys(t, r)
			_, bundle, state, material := keyState(t, r)
			oldID := state.PreparedIssuer
			short := editSigningCertificate(t, material.Keys[oldID], func(cert *x509.Certificate) {
				cert.NotAfter = state.ActivateAt.Add(r.Config.Rotation.Interval + r.Config.CertificateLifetime - time.Second)
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

			writeSigningCredentials(t, r, bundle, state, material)
			before, _, _, _ := keyState(t, r)

			base := r.Client.(client.WithWatch)
			if fail {
				r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return wire.Unavailable
				}})
				if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err == nil {
					t.Fatal("failed replacement accepted")
				}

				after, _, _, _ := keyState(t, r)
				if !reflect.DeepEqual(before.Data, after.Data) {
					t.Fatal("failed stale replacement partially changed credentials")
				}

				r.Client = base
			}

			runKeys(t, r)

			_, after, replacement, keys := keyState(t, r)
			if after.Generation != bundle.Generation+1 || replacement.PreparedIssuer == shortID || replacement.ActiveIssuer != initial.ActiveIssuer || !replacement.ActivateAt.Equal(now.Add(r.Config.Rotation.PrepareFor)) {
				t.Fatal("stale preparation replacement reset active issuer or publication version")
			}

			if containsRoot(after, shortID) || len(keys.Keys) != len(after.PeerTrustRoots) {
				t.Fatal("stale root and private material not replaced atomically")
			}

			if _, retained := keys.Keys[shortID]; retained {
				t.Fatal("stale private key retained")
			}

			for _, key := range after.CacheKeys {
				if key.State == wire.PreparedKey && binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(after.Generation) {
					t.Fatal("replacement prepared key has wrong creation generation")
				}
			}
		})
	}
}
