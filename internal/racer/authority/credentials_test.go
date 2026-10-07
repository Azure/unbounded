// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
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
			if version.Annotations[credentialClaim] != "" || version.Annotations[credentialUID] != "" || secret.Annotations[credentialClaim] != want {
				t.Fatal("candidate claim missing or parent committed before Create")
			}
		}

		return c.Create(ctx, obj, opts...)
	}})
	runKeys(t, r)
	runKeys(t, r)
	version, _, err := readVersion(t.Context(), base, r.Config)
	require.NoError(t, err)

	secret := &corev1.Secret{}
	require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}, secret))
	require.NotEmpty(t, secret.UID)
	require.Equal(t, string(secret.UID), version.Annotations[credentialUID])
	require.Equal(t, secret.Annotations[credentialClaim], version.Annotations[credentialClaim])

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

			r.Client = rejectWrites(t, base)
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
		assertPreparedCredentials(t, secret)

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

func assertPreparedCredentials(t *testing.T, secret *corev1.Secret) {
	t.Helper()

	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	require.NoError(t, err)

	var (
		rotation RotationState
		material issuerMaterial
	)

	require.NoError(t, json.Unmarshal(secret.Data["rotation.json"], &rotation), "unreadable candidate rotation")
	require.NoError(t, json.Unmarshal(secret.Data["issuer.json"], &material), "unreadable candidate issuers")
	candidate := credentialState{bundle: bundle, rotation: rotation, material: material}
	require.NoError(t, candidate.validateRotation(), "incoherent candidate")
	require.EqualValues(t, 2, bundle.Generation)
	require.NotEmpty(t, rotation.PreparedIssuer)
	require.Len(t, material.Keys, 2)

	for _, key := range bundle.CacheKeys {
		require.LessOrEqual(t, binary.BigEndian.Uint64(key.Key.ID[4:12]), uint64(bundle.Generation), "creation generation exceeds publication")
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

			rebindSigning(&bundle, &state, material, oldID, short, true)

			bundle.Generation++
			writeSigningCredentials(t, r, bundle, state, material)
			before, _, _, _ := keyState(t, r)

			base := r.Client.(client.WithWatch)
			if fail {
				r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return wire.Unavailable
				}})
				_, err := r.Reconcile(t.Context(), ctrl.Request{})
				require.Error(t, err, "failed replacement accepted")

				after, _, _, _ := keyState(t, r)
				require.Equal(t, before.Data, after.Data, "failed stale replacement partially changed credentials")

				r.Client = base
			}

			runKeys(t, r)

			_, after, replacement, keys := keyState(t, r)
			if after.Generation != bundle.Generation+1 || replacement.PreparedIssuer == shortID || replacement.ActiveIssuer != initial.ActiveIssuer || !replacement.ActivateAt.Equal(now.Add(r.Config.Rotation.PrepareFor)) {
				t.Fatal("stale preparation replacement reset active issuer or publication version")
			}

			require.False(t, containsRoot(after, shortID), "stale root retained")
			require.Len(t, keys.Keys, len(after.PeerTrustRoots), "private material not replaced atomically")
			require.NotContains(t, keys.Keys, shortID, "stale private key retained")

			for _, key := range after.CacheKeys {
				if key.State == wire.PreparedKey && binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(after.Generation) {
					t.Fatal("replacement prepared key has wrong creation generation")
				}
			}
		})
	}
}

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
					require.NoError(t, err)
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(shared), shared))
					shared.Data["bundle.json"] = encoded
					require.NoError(t, r.Update(t.Context(), shared))
				}
				bundle.Generation = 2
				writeBundle(bundle)
				runKeys(t, r)

				accepted, _, err := r.Trust.keyring()
				require.NoError(t, err)
				require.EqualValues(t, 2, accepted.generation)

				digest := sha256.Sum256([]byte(accepted.encoded))

				candidate, err := wire.DecodeBundle(bytes.NewBufferString(accepted.encoded))
				require.NoError(t, err)

				if scenario == "rollback" {
					candidate.Generation--
				} else {
					key := candidate.CacheKeys[0]

					candidate.CacheKeys[0], err = wire.NewCacheKey(key.Key, key.State, [32]byte{1})
					require.NoError(t, err)
				}

				writeBundle(candidate)

				for range 3 {
					_, err := r.Reconcile(t.Context(), ctrl.Request{})
					require.ErrorIs(t, err, wire.Conflict, "replay was not rejected")
					_, _, err = r.Trust.keyring()
					require.ErrorIs(t, err, wire.Unavailable, "replay restored delivery")
					_, err = r.Trust.pool()
					require.ErrorIs(t, err, wire.Unavailable, "replay restored trust")
					require.EqualValues(t, 2, r.Trust.highWater)
					require.Equal(t, digest, r.Trust.digest, "invalidation lost accepted replay protection")
				}

				if restore == "new generation" {
					bundle = candidate
					bundle.Generation = 3
				}

				writeBundle(bundle)
				runKeys(t, r)

				current, _, err := r.Trust.keyring()
				require.NoError(t, err)
				require.Equal(t, bundle.Generation, current.generation)

				expected, err := wire.EncodeBundle(bundle)
				require.NoError(t, err)
				require.Equal(t, string(expected), current.encoded, "restored wrong content")

				_, err = r.Trust.pool()
				require.NoError(t, err, "restoration did not restore trust")
				require.Equal(t, bundle.Generation, r.Trust.highWater)
				require.Equal(t, sha256.Sum256(expected), r.Trust.digest, "restoration did not advance replay protection")
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
		require.Equal(t, "<redacted keyring>", fmt.Sprintf(format, accepted), "diagnostic exposed keyring")
	}

	assertRejectedKeyringInstalls(t, f, roots, accepted)

	older := wire.Generation(1)

	current, err := trust.waitKeyring(f.ctx, &older)
	if err != nil || current != accepted {
		t.Fatalf("older cursor did not return newest: %v", err)
	}
}

func assertRejectedKeyringInstalls(t *testing.T, f *servingFixture, roots *x509.CertPool, accepted *acceptedKeyring) {
	t.Helper()

	trust := f.a.Server.Trust

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

func testKeyring(t *testing.T) (*KeyringReconciler, *time.Time) {
	t.Helper()

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: types.UID(testNodeUID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	topology := initializedTopology(t, volume)
	a := Assemble(topology.Config, topology.Client, topology.APIReader)
	now := time.Now().UTC().Truncate(time.Second)
	a.Keyring.Now = func() time.Time { return now }

	return a.Keyring, &now
}

func runKeys(t *testing.T, r *KeyringReconciler) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return result
}

func keyState(t *testing.T, r *KeyringReconciler) (*corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial) {
	t.Helper()

	version, _, err := readVersion(context.Background(), r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	credentials, err := readBoundCredentials(context.Background(), r.APIReader, r.Config, version.Annotations[credentialClaim], version)
	if err != nil {
		t.Fatal(err)
	}

	return credentials.secret, credentials.bundle, credentials.rotation, credentials.material
}

func TestKeyringRotationLifecycle(t *testing.T) {
	r, now := testKeyring(t)
	started := *now
	result := runKeys(t, r)

	shared, initial, state, _ := keyState(t, r)
	require.EqualValues(t, 1, initial.Generation)
	require.Len(t, initial.CacheKeys, 2)
	require.Len(t, initial.PeerTrustRoots, 1)
	require.Equal(t, r.Config.Rotation.Interval-r.Config.Rotation.PrepareFor, result.RequeueAfter)
	require.True(t, trustReady(r.Trust), "initial readiness")

	assertCacheKeyGenerations(t, initial.CacheKeys, 1, 1)
	// Repeated reconciliation must neither write nor consume a generation.
	runKeys(t, r)

	unchanged, _, _, _ := keyState(t, r)
	if shared.ResourceVersion != unchanged.ResourceVersion {
		t.Fatal("unchanged credentials written")
	}

	*now = state.NextRotation
	result = runKeys(t, r)

	_, staged, prepared, _ := keyState(t, r)
	if !prepared.ActivateAt.Equal(started.Add(r.Config.Rotation.Interval)) {
		t.Fatal("activation cadence must include the preparation interval")
	}

	require.EqualValues(t, 2, staged.Generation)
	require.Len(t, staged.CacheKeys, 4)
	require.Len(t, staged.PeerTrustRoots, 2)
	require.Equal(t, state.ActiveIssuer, prepared.ActiveIssuer)
	require.NotEmpty(t, prepared.PreparedIssuer)
	require.Equal(t, r.Config.Rotation.PrepareFor, result.RequeueAfter)

	assertCacheKeyGenerations(t, staged.CacheKeys, 1, 2)

	if !reflect.DeepEqual(staged.CacheKeys[:len(initial.CacheKeys)], initial.CacheKeys) {
		t.Fatal("staging changed retained keys")
	}
	// A fresh process resumes the persisted deadline, not a new delay.
	restarted := Assemble(r.Config, r.Client, r.APIReader).Keyring
	restarted.Now = r.Now

	*now = prepared.ActivateAt.Add(-time.Second)
	if got := runKeys(t, restarted).RequeueAfter; got != time.Second {
		t.Fatalf("lost activation deadline: %v", got)
	}

	*now = prepared.ActivateAt

	runKeys(t, restarted)

	_, activated, active, _ := keyState(t, r)
	require.EqualValues(t, 3, activated.Generation)
	require.Equal(t, prepared.PreparedIssuer, active.ActiveIssuer)
	require.Empty(t, active.PreparedIssuer)
	require.Len(t, active.Retiring, 1)
	require.Len(t, activated.CacheKeys, 2)

	assertActivatedCacheKeys(t, activated.CacheKeys, staged.CacheKeys[2:])
	// Further cycles overlap without evicting an earlier retirement prematurely.
	*now = active.NextRotation

	runKeys(t, restarted)
	_, _, nextStage, _ := keyState(t, r)
	*now = nextStage.ActivateAt

	runKeys(t, restarted)

	_, overlap, overlapping, _ := keyState(t, r)
	if len(overlap.PeerTrustRoots) != 3 || len(overlap.CacheKeys) != 2 {
		t.Fatal("multiple retiring generations lost")
	}

	*now = active.Retiring[state.ActiveIssuer]

	runKeys(t, restarted)

	_, pruned, prunedState, material := keyState(t, r)
	if containsRoot(pruned, state.ActiveIssuer) || len(pruned.CacheKeys) != 4 || len(material.Keys) != 3 || !prunedState.Retiring[active.ActiveIssuer].Equal(overlapping.Retiring[active.ActiveIssuer]) {
		t.Fatal("retirement pruning/reset")
	}
	// Topology CAS preserves the one-way initialization claim.
	topology := Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, topology, context.Background())
	keyState(t, r)
}

func assertCacheKeyGenerations(t *testing.T, keys []wire.CacheKey, active, prepared uint64) {
	t.Helper()

	for _, k := range keys {
		want := active
		if active == prepared {
			require.Equal(t, wire.ActiveKey, k.State, "initial key not active")
		}

		if k.State == wire.PreparedKey {
			want = prepared
		}

		require.Equal(t, "RKG1", string(k.Key.ID[:4]))
		require.Equal(t, want, binary.BigEndian.Uint64(k.Key.ID[4:12]), "key creation generation changed")
	}
}

func assertActivatedCacheKeys(t *testing.T, activated, staged []wire.CacheKey) {
	t.Helper()

	for i, k := range activated {
		require.NotEqual(t, wire.PreparedKey, k.State, "prepared key not activated")
		require.Equal(t, staged[i].Key, k.Key, "activation changed key identity")
		require.True(t, k.EqualMaterial(staged[i]), "activation changed key material")
	}
}

func TestKeyringDerivedDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		retain   time.Duration
		steps    []struct{ at, next time.Duration }
	}{
		{
			name:     "overlapping retirements",
			interval: 24 * time.Hour,
			retain:   48 * time.Hour,
			steps: []struct{ at, next time.Duration }{
				{0, 23 * time.Hour},
				{23 * time.Hour, 24 * time.Hour},
				{24 * time.Hour, 47 * time.Hour},
				{47 * time.Hour, 48 * time.Hour},
				{48 * time.Hour, 71 * time.Hour},
				{71 * time.Hour, 72 * time.Hour},
			},
		},
		{
			name:     "retirement during preparation",
			interval: 24 * time.Hour,
			retain:   24*time.Hour + 30*time.Minute,
			steps: []struct{ at, next time.Duration }{
				{0, 23 * time.Hour},
				{23 * time.Hour, 24 * time.Hour},
				{24 * time.Hour, 47 * time.Hour},
				{47 * time.Hour, 48 * time.Hour},
				{48 * time.Hour, 48*time.Hour + 30*time.Minute},
				{48*time.Hour + 30*time.Minute, 71 * time.Hour},
			},
		},
		{
			name:     "retirement before next rotation",
			interval: 7 * 24 * time.Hour,
			retain:   24 * time.Hour,
			steps: []struct{ at, next time.Duration }{
				{0, 167 * time.Hour},
				{167 * time.Hour, 168 * time.Hour},
				{168 * time.Hour, 192 * time.Hour},
				{192 * time.Hour, 335 * time.Hour},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.Rotation = RotationPolicy{Interval: tc.interval, PrepareFor: time.Hour, RetainFor: tc.retain}
			start := *now

			for i, step := range tc.steps {
				*now = start.Add(step.at)
				result := runKeys(t, r)

				shared, bundle, state, _ := keyState(t, r)
				if want := start.Add(step.next); !state.nextTransition().Equal(want) || result.RequeueAfter != want.Sub(*now) {
					t.Fatalf("step %d: deadline=%v requeue=%v, want %v", i, state.nextTransition(), result.RequeueAfter, want)
				}

				if bundle.Generation != wire.Generation(i+1) {
					t.Fatalf("step %d did not publish exactly one transition: generation=%d", i, bundle.Generation)
				}

				var persisted map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(shared.Data["rotation.json"], &persisted))
				require.NotContains(t, persisted, "next_transition", "derived deadline persisted")

				// Every phase must resume from only primary state without rewriting
				// credentials or extending a deadline when the process restarts.
				restarted := Assemble(r.Config, r.Client, r.APIReader).Keyring
				restarted.Now = r.Now

				*now = now.Add(time.Second)
				require.Equal(t, step.next-step.at-time.Second, runKeys(t, restarted).RequeueAfter, "step %d: restart requeue", i)

				unchanged, _, _, _ := keyState(t, restarted)
				require.Equal(t, shared.ResourceVersion, unchanged.ResourceVersion, "step %d: restart rewrote credentials", i)
				require.Equal(t, shared.Data, unchanged.Data, "step %d: restart rewrote credentials", i)

				r = restarted
			}
		})
	}
}

func TestGenerationBoundCacheKey(t *testing.T) {
	if _, err := newCacheKey(wire.CacheID(testNodeUID), wire.PageKey, wire.ActiveKey, 0); err == nil {
		t.Fatal("zero or wrapped generation accepted")
	}

	for _, generation := range []wire.Generation{1, 256, math.MaxUint64} {
		key, err := newCacheKey(wire.CacheID(testNodeUID), wire.PageKey, wire.PreparedKey, generation)
		if err != nil {
			t.Fatal(err)
		}

		if len(key.Key.ID) != 16 || string(key.Key.ID[:4]) != "RKG1" || binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(generation) {
			t.Fatalf("creation generation not preserved: %x", key.Key.ID)
		}
	}
}

func TestPlanRotationExhaustedKeyCreation(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, b, state, _ := keyState(t, r)
	b.Generation = math.MaxUint64
	catalog := []wire.CacheDefinition{{ID: wire.CacheID(testNodeUID)}}

	next, nextState, err := PlanRotation(r.Config.Rotation, b, state, catalog, *now)
	if err != nil || !reflect.DeepEqual(next, b) || !reflect.DeepEqual(nextState, state) {
		t.Fatalf("exhausted idle plan: %v", err)
	}

	catalog = append(catalog, wire.CacheDefinition{ID: wire.CacheID(testOtherUID)})
	if _, _, err := PlanRotation(r.Config.Rotation, b, state, catalog, *now); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("exhausted admission plan: %v", err)
	}

	state.PreparedIssuer = state.ActiveIssuer
	if _, _, err := PlanRotation(r.Config.Rotation, b, state, catalog[:1], state.NextRotation); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("exhausted staging plan: %v", err)
	}
}

func TestKeyringCatalogAndBounds(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	shared, original, s, _ := keyState(t, r)
	// Active-only fits, but staging the same catalog must account for overlap.
	var catalog []wire.CacheDefinition
	for n := range 800 {
		catalog = append(catalog, wire.CacheDefinition{ID: wire.CacheID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", n+1))})
	}

	b, state, err := PlanRotation(r.Config.Rotation, original, s, catalog, *now)
	if err != nil {
		t.Fatal(err)
	}

	state.PreparedIssuer = state.ActiveIssuer
	if _, _, err := PlanRotation(r.Config.Rotation, b, state, catalog, state.NextRotation); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("overlap bound not enforced: %v", err)
	}

	encoded, err := wire.EncodeBundle(original)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(encoded, shared.Data["bundle.json"]) {
		t.Fatal("planner mutated input")
	}

	volume := &racerv1.ClusterVolume{}
	if err := r.APIReader.Get(context.Background(), client.ObjectKey{Name: "cache"}, volume); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(context.Background(), volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, removed, _, _ := keyState(t, r)
	if len(removed.CacheKeys) != 0 || removed.Generation != 2 {
		t.Fatal("removed cache keys retained")
	}

	volume.ResourceVersion = ""

	volume.UID = types.UID(testOtherUID)
	if err := r.Create(context.Background(), volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, recreated, _, _ := keyState(t, r)
	for _, key := range recreated.CacheKeys {
		if key.Key.Cache != wire.CacheID(testOtherUID) || key.EqualMaterial(original.CacheKeys[0]) {
			t.Fatal("recreated cache inherited keys")
		}
	}
}

func TestPlanRotationOwnsOutput(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)
	_, _, prepared, _ := keyState(t, r)
	*now = prepared.ActivateAt

	runKeys(t, r)
	_, original, state, _ := keyState(t, r)
	catalog := []wire.CacheDefinition{{ID: wire.CacheID(testNodeUID)}}
	// The old codec round trip preserves order, even when it is not sorted.
	original.PeerTrustRoots[0], original.PeerTrustRoots[1] = original.PeerTrustRoots[1], original.PeerTrustRoots[0]
	original.CacheKeys[0], original.CacheKeys[1] = original.CacheKeys[1], original.CacheKeys[0]

	before, err := wire.EncodeBundle(original)
	if err != nil {
		t.Fatal(err)
	}

	beforeState, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	want, err := wire.DecodeBundle(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}

	next, nextState, err := PlanRotation(r.Config.Rotation, original, state, catalog, *now)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(next, want) || !reflect.DeepEqual(nextState, state) {
		t.Fatal("idle planner changed bundle representation or rotation state")
	}

	// Exercise both outer collections and nested bytes, plus the retirement map.
	next.PeerTrustRoots[0][0] ^= 0xff
	next.PeerTrustRoots[1] = nil
	next.CacheKeys[0].Key.ID[0] ^= 0xff
	next.CacheKeys[1].State = wire.PreparedKey

	for id := range nextState.Retiring {
		delete(nextState.Retiring, id)
	}

	after, err := wire.EncodeBundle(original)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("output aliases input bundle: %v", err)
	}

	afterState, err := json.Marshal(state)
	if err != nil || !bytes.Equal(beforeState, afterState) {
		t.Fatalf("output aliases input retirement map: %v", err)
	}
}

func TestPlanRotationInputValidation(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)

	_, original, state, _ := keyState(t, r)
	for _, tc := range []struct {
		name string
		edit func(*wire.KeyringBundle)
		want error
	}{
		{"schema", func(b *wire.KeyringBundle) { b.SchemaVersion++ }, wire.UnsupportedVersion},
		{"generation", func(b *wire.KeyringBundle) { b.Generation = 0 }, wire.InvalidRequest},
		{"duplicate root", func(b *wire.KeyringBundle) { b.PeerTrustRoots = append(b.PeerTrustRoots, b.PeerTrustRoots[0]) }, wire.InvalidRequest},
		{"invalid key", func(b *wire.KeyringBundle) {
			b.CacheKeys = append([]wire.CacheKey(nil), b.CacheKeys...)
			b.CacheKeys[0].Key.ID = nil
		}, wire.InvalidRequest},
		{"encoded size", func(b *wire.KeyringBundle) {
			key := b.CacheKeys[0]

			b.CacheKeys = make([]wire.CacheKey, 4000)
			for i := range b.CacheKeys {
				ref := key.Key
				ref.Cache = wire.CacheID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i+1))

				var material [32]byte
				binary.BigEndian.PutUint64(material[:8], uint64(i+1))

				var err error

				b.CacheKeys[i], err = wire.NewCacheKey(ref, key.State, material)
				if err != nil {
					t.Fatal(err)
				}
			}
		}, wire.TooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := original
			tc.edit(&b)
			// An empty catalog would discard all keys; invalid/oversized input
			// must still fail before the planner can shrink it into a valid output.
			if _, _, err := PlanRotation(r.Config.Rotation, b, state, nil, *now); !errors.Is(err, tc.want) {
				t.Fatalf("input validation: got %v, want %v", err, tc.want)
			}
		})
	}

	original.CacheKeys = nil
	state.Retiring = nil

	next, nextState, err := PlanRotation(r.Config.Rotation, original, state, nil, *now)
	if err != nil || next.CacheKeys == nil || nextState.Retiring == nil {
		t.Fatalf("empty collection normalization: %v", err)
	}
}

func TestKeyringCorruptionAndGenerationExhaustion(t *testing.T) {
	for _, corrupt := range []string{"timestamp", "transition mismatch", "missing activation", "missing prepared issuer", "zero root retirement", "zero key retirement", "missing root retirement", "missing key retirement", "replaced retirement", "nil retirement", "unknown retirement", "active issuer", "bundle", "private key", "generation", "binding"} {
		t.Run(corrupt, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)

			prepareCorruptionPhase(t, r, now, corrupt)

			shared, b, s, _ := keyState(t, r)
			corruptCredentials(corrupt, shared, b, s, now)
			assertCorruptCredentialsUnchanged(t, r, shared)
		})
	}
}

func corruptCredentials(corrupt string, shared *corev1.Secret, b wire.KeyringBundle, s RotationState, now *time.Time) {
	switch corrupt {
	case "timestamp":
		s.NextRotation = time.Time{}
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "transition mismatch":
		// Preparation must activate strictly after the rotation timestamp.
		s.ActivateAt = s.NextRotation
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "missing activation":
		s.ActivateAt = time.Time{}
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "missing prepared issuer":
		s.PreparedIssuer = ""
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "zero root retirement", "missing root retirement", "replaced retirement":
		corruptRootRetirement(corrupt, b, &s)

		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "zero key retirement", "missing key retirement":
		// Symmetric retirement metadata is never valid, with or without a deadline.
		s.Retiring[keyID(b.CacheKeys[0])] = time.Time{}
		if corrupt == "missing key retirement" {
			s.Retiring[keyID(b.CacheKeys[0])] = s.NextRotation
		}

		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "nil retirement":
		s.Retiring = nil
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "unknown retirement":
		s.Retiring["unknown"] = s.NextRotation.Add(time.Hour)
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "active issuer":
		s.ActiveIssuer = "missing"
		shared.Data["rotation.json"], _ = json.Marshal(s)
	case "bundle":
		shared.Data["bundle.json"] = []byte("{}")
	case "generation":
		b.Generation = math.MaxUint64
		shared.Data["bundle.json"], _ = wire.EncodeBundle(b)
		*now = s.NextRotation
	case "binding":
		shared.Annotations[credentialClaim] = "foreign"
	case "private key":
		shared.Data["issuer.json"] = []byte("{}")
	}
}

func prepareCorruptionPhase(t *testing.T, r *KeyringReconciler, now *time.Time, corrupt string) {
	t.Helper()

	switch corrupt {
	case "transition mismatch", "missing activation", "missing prepared issuer", "zero root retirement", "zero key retirement", "missing root retirement", "missing key retirement", "replaced retirement", "unknown retirement":
		_, _, initial, _ := keyState(t, r)
		*now = initial.NextRotation

		runKeys(t, r)
	}

	switch corrupt {
	case "zero root retirement", "zero key retirement", "missing root retirement", "missing key retirement", "replaced retirement", "unknown retirement":
		_, _, prepared, _ := keyState(t, r)
		*now = prepared.ActivateAt

		runKeys(t, r)
	}
}

func corruptRootRetirement(corrupt string, b wire.KeyringBundle, s *RotationState) {
	for _, root := range b.PeerTrustRoots {
		id := rootID(root)
		if id == s.ActiveIssuer {
			continue
		}

		switch corrupt {
		case "zero root retirement":
			s.Retiring[id] = time.Time{}
		case "missing root retirement":
			delete(s.Retiring, id)
		case "replaced retirement":
			s.Retiring[s.ActiveIssuer] = s.Retiring[id]
			delete(s.Retiring, id)
		}
	}
}

func assertCorruptCredentialsUnchanged(t *testing.T, r *KeyringReconciler, shared *corev1.Secret) {
	t.Helper()
	require.NoError(t, r.Update(t.Context(), shared))
	before := shared.DeepCopy()
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.Error(t, err, "corrupt state accepted")
	require.False(t, trustReady(r.Trust), "corrupt state trusted")

	after := &corev1.Secret{}
	require.NoError(t, r.APIReader.Get(t.Context(), client.ObjectKeyFromObject(shared), after))
	require.Equal(t, before.Data, after.Data, "corrupt state rewritten")
}

func TestKeyringExhaustedGenerationTransitions(t *testing.T) {
	for _, transition := range []string{"idle", "admission", "stage", "empty stage", "activate", "remove", "prune"} {
		t.Run(transition, func(t *testing.T) {
			r, now := testKeyring(t)

			r.Config.Rotation.Interval = 7 * 24 * time.Hour
			if transition == "empty stage" {
				require.NoError(t, r.Delete(t.Context(), &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}))
			}

			runKeys(t, r)

			_, _, initial, _ := keyState(t, r)
			if transition == "activate" || transition == "prune" {
				*now = initial.NextRotation

				runKeys(t, r)
				_, _, staged, _ := keyState(t, r)
				*now = staged.ActivateAt

				if transition == "prune" {
					runKeys(t, r)
					_, _, active, _ := keyState(t, r)
					*now = active.Retiring[initial.ActiveIssuer]
				}
			}

			shared, b, _, _ := keyState(t, r)
			b.Generation = math.MaxUint64

			var err error

			shared.Data["bundle.json"], err = wire.EncodeBundle(b)
			require.NoError(t, err)
			require.NoError(t, r.Update(t.Context(), shared))

			switch transition {
			case "admission":
				volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "added", UID: types.UID(testOtherUID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
				require.NoError(t, r.Create(t.Context(), volume))
			case "stage", "empty stage":
				*now = initial.NextRotation
			case "remove":
				require.NoError(t, r.Delete(t.Context(), &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}))
			}

			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				t.Fatal("exhausted generation wrote durable state")
				return nil
			}})
			if transition == "idle" {
				runKeys(t, r)
			} else if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("exhausted generation transition: %v", err)
			}

			after, preserved, _, _ := keyState(t, r)
			require.Equal(t, shared.ResourceVersion, after.ResourceVersion, "exhausted generation wrote credentials")
			require.Equal(t, b, preserved, "exhausted generation changed the published bundle")
		})
	}
}

func TestKeyringAdmissionAtLastGeneration(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	shared, b, _, _ := keyState(t, r)
	b.Generation = math.MaxUint64 - 1

	var err error

	shared.Data["bundle.json"], err = wire.EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Update(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "added", UID: types.UID(testOtherUID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	if err := r.Create(t.Context(), volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, admitted, _, _ := keyState(t, r)
	if admitted.Generation != math.MaxUint64 || len(admitted.CacheKeys) != 4 || !reflect.DeepEqual(admitted.CacheKeys[:2], b.CacheKeys) {
		t.Fatal("last generation admission lost existing keys or new scopes")
	}

	for _, key := range admitted.CacheKeys[2:] {
		if key.Key.Cache != wire.CacheID(testOtherUID) || key.State != wire.ActiveKey || binary.BigEndian.Uint64(key.Key.ID[4:12]) != math.MaxUint64 {
			t.Fatal("admitted key did not bind the last publication generation")
		}
	}

	runKeys(t, r)
}

func TestKeyringOversizedOverlapDoesNotWrite(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, initial, _, _ := keyState(t, r)

	for n := range 800 {
		volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cache-%d", n), UID: types.UID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", n+1))}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
		if err := r.Create(context.Background(), volume); err != nil {
			t.Fatal(err)
		}
	}

	runKeys(t, r)
	shared, admitted, state, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, admitted)
	if err != nil || len(admitted.CacheKeys) != 2*capacity || capacity >= 801 || !trustReady(r.Trust) {
		t.Fatalf("rotation capacity not enforced: %d, %v", capacity, err)
	}

	for _, key := range initial.CacheKeys {
		found := false
		for _, accepted := range admitted.CacheKeys {
			found = found || key.EqualMaterial(accepted)
		}

		if !found {
			t.Fatal("growth evicted an existing cache key")
		}
	}

	base := r.Client
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		t.Fatal("rejected growth wrote durable state")
		return nil
	}})
	runKeys(t, r)

	unchanged, _, _, _ := keyState(t, r)
	if unchanged.ResourceVersion != shared.ResourceVersion {
		t.Fatal("rejected growth consumed generation")
	}

	r.Client = base
	*now = state.NextRotation

	runKeys(t, r)

	_, staged, _, _ := keyState(t, r)
	if len(staged.CacheKeys) != 4*capacity || !trustReady(r.Trust) {
		t.Fatal("admitted catalog could not rotate")
	}
}

func TestKeyringEmptyCatalogAndCacheAddedDuringPreparation(t *testing.T) {
	r, now := testKeyring(t)

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache"}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	if err := r.Delete(context.Background(), volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, b, s, _ := keyState(t, r)
	if b.Generation != 1 || len(b.CacheKeys) != 0 {
		t.Fatal("empty catalog initialization has wrong generation or keys")
	}

	*now = s.NextRotation

	runKeys(t, r)
	_, _, staged, _ := keyState(t, r)

	volume.UID = types.UID(testNodeUID)
	if err := r.Create(context.Background(), volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, added, _, _ := keyState(t, r)
	if added.Generation != 3 || len(added.CacheKeys) != 2 {
		t.Fatal("new cache missing initial keys")
	}

	for _, key := range added.CacheKeys {
		if binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(added.Generation) {
			t.Fatal("new cache key did not bind its admission generation")
		}
	}

	*now = staged.ActivateAt

	runKeys(t, r)

	_, activated, _, _ := keyState(t, r)
	for n, key := range activated.CacheKeys {
		if key.State != wire.ActiveKey || !reflect.DeepEqual(key.Key, added.CacheKeys[n].Key) || !key.EqualMaterial(added.CacheKeys[n]) {
			t.Fatal("new cache key retired without replacement")
		}
	}
}

func TestKeyringRotationCrashRecovery(t *testing.T) {
	for _, phase := range []string{"stage", "activate", "prune"} {
		for _, failure := range []string{"before issuer", "after issuer", "before bundle", "after bundle"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				exerciseRotationCrash(t, phase, failure)
			})
		}
	}
}

func exerciseRotationCrash(t *testing.T, phase, failure string) {
	t.Helper()
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	if phase != "stage" {
		runKeys(t, r)
		_, _, staged, _ := keyState(t, r)
		*now = staged.ActivateAt

		if phase == "prune" {
			runKeys(t, r)
			*now = now.Add(r.Config.Rotation.RetainFor)
		}
	}

	base := r.Client.(client.WithWatch)
	boom := errors.New("lost response")
	failed := false
	original, _, _, _ := keyState(t, r)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		require.Equal(t, r.Config.CredentialsSecretName, obj.GetName(), "rotation wrote outside the credentials CAS")

		if strings.HasPrefix(failure, "before ") && !failed {
			failed = true
			return boom
		}

		if err := c.Update(ctx, obj, opts...); err != nil {
			return err
		}

		if strings.HasPrefix(failure, "after ") && !failed {
			failed = true
			return boom
		}

		return nil
	}})

	_, err := r.Reconcile(context.Background(), ctrl.Request{})

	require.True(t, failed, "write failure not injected")
	require.ErrorIs(t, err, boom, "write failure accepted")
	require.False(t, trustReady(r.Trust), "failed write trusted")
	// Former two-Secret boundaries now fail one coherent atomic version.
	committed, before, beforeState, _ := keyState(t, r)
	if strings.HasPrefix(failure, "before ") && !reflect.DeepEqual(original.Data, committed.Data) {
		t.Fatal("failed CAS partially changed credentials")
	}

	recovered := Assemble(r.Config, base, base).Keyring
	recovered.Now = r.Now
	runKeys(t, recovered)

	_, after, afterState, _ := keyState(t, recovered)
	require.GreaterOrEqual(t, after.Generation, before.Generation, "generation reset")

	if strings.HasPrefix(failure, "after ") {
		require.Equal(t, before.Generation, after.Generation, "committed atomic generation replaced on recovery")
		require.Equal(t, beforeState, afterState, "committed atomic state replaced on recovery")
	}

	if !beforeState.ActivateAt.IsZero() && !afterState.ActivateAt.IsZero() && !beforeState.ActivateAt.Equal(afterState.ActivateAt) {
		t.Fatal("committed preparation restarted")
	}
}

func TestKeyringPrivatePruneRecovery(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("response-lost=%t", afterWrite), func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.Rotation.Interval = 7 * 24 * time.Hour
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			runKeys(t, r)
			_, _, staged, _ := keyState(t, r)
			*now = staged.ActivateAt

			runKeys(t, r)
			_, _, active, _ := keyState(t, r)
			*now = active.Retiring[initial.ActiveIssuer]
			base := r.Client.(client.WithWatch)
			boom := errors.New("private prune interrupted")

			r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetName() != r.Config.CredentialsSecretName {
					return c.Update(ctx, obj, opts...)
				}

				_, b, _, _ := keyState(t, r)
				require.True(t, containsRoot(b, initial.ActiveIssuer), "root changed before atomic pruning")

				if afterWrite {
					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}
				}

				return boom
			}})
			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.ErrorIs(t, err, boom, "prune interruption")

			_, before, _, beforeMaterial := keyState(t, r)
			if containsRoot(before, initial.ActiveIssuer) == afterWrite || (len(beforeMaterial.Keys) == 2) == afterWrite {
				t.Fatal("root and private key cleanup were not atomic")
			}

			r.Client = base
			runKeys(t, r)

			_, after, _, material := keyState(t, r)

			wantGeneration := before.Generation
			if !afterWrite {
				wantGeneration++
			}

			if wantGeneration != after.Generation || len(material.Keys) != 1 || containsRoot(after, initial.ActiveIssuer) {
				t.Fatal("prune recovery changed publication or retained private material")
			}
		})
	}
}

func TestKeyringBundleConflictKeepsPendingIssuer(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == r.Config.CredentialsSecretName {
			return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), wire.Conflict)
		}

		return c.Update(ctx, obj, opts...)
	}})

	result, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil || result.RequeueAfter != retryConflictDelay {
		t.Fatalf("bundle conflict: %v, %v", result, err)
	}

	_, b, _, material := keyState(t, r)
	if b.Generation != 1 || len(material.Keys) != 1 || !containsRoot(b, initial.ActiveIssuer) {
		t.Fatal("conflicting bundle became authoritative")
	}

	r.Client = base
	runKeys(t, r)

	_, b, state, _ := keyState(t, r)
	if state.PreparedIssuer == "" || state.PreparedIssuer == initial.ActiveIssuer || b.Generation != 2 {
		t.Fatal("conflict recovery did not publish a coherent preparation")
	}
}

func TestKeyringInitializationNeverResurrects(t *testing.T) {
	for _, failure := range []string{"before claim", "after claim", "before issuer", "after issuer", "before bundle", "after bundle"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := testKeyring(t)
			base := r.Client.(client.WithWatch)
			boundary := strings.NewReplacer("claim", "commit", "issuer", "create", "bundle", "create").Replace(failure)
			r.Client = interruptInitialization(base, boundary, func() {})
			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.ErrorIs(t, err, errInitializationInterrupted, "crash not injected")

			recovered := Assemble(r.Config, base, base).Keyring
			recovered.Now = r.Now

			_, err = recovered.Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err, "staged initialization must recover an uncommitted candidate")
			version, _, err := readVersion(t.Context(), base, r.Config)
			require.NoError(t, err)

			secret := &corev1.Secret{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}, secret))
			require.NotEmpty(t, secret.UID)
			require.Equal(t, string(secret.UID), version.Annotations[credentialUID])
		})
	}

	for _, lost := range []string{"issuer", "bundle", "both", "version", "marker"} {
		t.Run("lost "+lost, func(t *testing.T) {
			r, _ := testKeyring(t)
			issuer := testIssuer(r)
			runKeys(t, r)

			if lost == "both" || lost == "issuer" || lost == "bundle" {
				require.NoError(t, r.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}}))
			}

			if lost == "version" || lost == "marker" {
				name := r.Config.VersionConfigMapName
				if lost == "marker" {
					name = r.Config.InstallationConfigMapName
				}

				require.NoError(t, r.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: name}}))
			}

			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				t.Fatal("recreated established state")
				return nil
			}})
			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.Error(t, err, "lost state accepted")
			require.False(t, trustReady(r.Trust))

			_, err = issuer.TrustRoots(context.Background())
			require.Error(t, err, "lost state still trusted")
		})
	}
}

func TestKeyringConflictCancellationAndAuthoritativeReads(t *testing.T) {
	for _, cancelAt := range []string{"none", "before", "issuer", "bundle"} {
		t.Run(cancelAt, func(t *testing.T) {
			exerciseKeyringCancellation(t, cancelAt)
		})
	}
}

func exerciseKeyringCancellation(t *testing.T, cancelAt string) {
	t.Helper()
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, s, _ := keyState(t, r)
	*now = s.NextRotation

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := r.Client.(client.WithWatch)
	writes := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("cached credential read")
			return nil
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("cached catalog read")
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++

			if (cancelAt == "issuer" || cancelAt == "bundle") && obj.GetName() == r.Config.CredentialsSecretName {
				cancel()
			}

			if cancelAt == "none" {
				return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), wire.Conflict)
			}

			return c.Update(ctx, obj, opts...)
		},
	})

	if cancelAt == "before" {
		cancel()
	}

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if cancelAt == "none" {
		require.NoError(t, err)
		require.Equal(t, retryConflictDelay, result.RequeueAfter, "conflict not requeued")
	} else {
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, reconcile.TerminalError(nil))
		require.Zero(t, result.RequeueAfter, "cancellation retried")
	}

	if cancelAt == "before" && writes != 0 || cancelAt == "issuer" && writes != 1 {
		t.Fatal("write after cancellation")
	}

	// Cancellation before admission observes no authority failure.
	if trustReady(r.Trust) != (cancelAt == "before") {
		t.Fatal("readiness did not reflect whether admission observed a failure")
	}

	r.Client = base
	runKeys(t, r)
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

func TestReplicaAuthenticationBindings(t *testing.T) {
	for _, scenario := range []string{"valid", "missing bearer", "review unavailable", "wrong account", "missing binding", "missing pod", "wrong pod uid", "failed pod", "missing account", "wrong account uid", "invalid expiration"} {
		t.Run(scenario, func(t *testing.T) {
			f, status, token := authenticatedBootstrapFixture(t)
			a := f.a.authority
			base := f.a.Topology.Client.(client.WithWatch)
			a.config.ControllerServiceAccount = a.config.DataplaneServiceAccount
			status.Audiences = []string{ReplicationAudience}
			want := mutateReplicaBinding(t, a, base, scenario, &status, &token)
			a.client = interceptor.NewClient(base, interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				if scenario == "review unavailable" {
					return wire.Unavailable
				}

				review := obj.(*authv1.TokenReview)
				require.Equal(t, []string{ReplicationAudience}, review.Spec.Audiences)
				review.Status = status

				return nil
			}})

			request := httptest.NewRequest("GET", "/replica", nil)
			if scenario != "missing bearer" {
				request.Header.Set("Authorization", "Bearer "+token)
			}

			identity, err := a.AuthenticateReplica(t.Context(), request)
			if want != nil {
				require.ErrorIs(t, err, want)
				require.Equal(t, ReplicaIdentity{}, identity)

				return
			}

			require.NoError(t, err)
			require.Equal(t, "pod-uid", identity.UID())
			require.True(t, identity.Expires().After(time.Now()))
			require.Same(t, a, identity.owner)
		})
	}
}

func mutateReplicaBinding(t *testing.T, a *Authority, c client.Client, scenario string, status *authv1.TokenReviewStatus, token *string) error {
	t.Helper()

	switch scenario {
	case "valid":
		return nil
	case "missing bearer", "missing binding", "invalid expiration":
		if scenario == "missing binding" {
			status.User.UID = ""
		}

		if scenario == "invalid expiration" {
			*token = "invalid"
		}

		return wire.Unauthenticated
	case "review unavailable":
		return wire.Unavailable
	case "wrong account":
		status.User.Username = "other"
	case "missing pod":
		status.User.Extra["authentication.kubernetes.io/pod-name"] = authv1.ExtraValue{"missing"}
	case "wrong pod uid":
		status.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"other"}
	case "failed pod":
		var pod corev1.Pod
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: a.config.Namespace, Name: singleExtra(status.User, "pod-name")}, &pod))
		pod.Status.Phase = corev1.PodFailed
		require.NoError(t, c.Status().Update(t.Context(), &pod))
	case "missing account":
		require.NoError(t, c.Delete(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: a.config.Namespace, Name: a.config.ControllerServiceAccount}}))
	case "wrong account uid":
		status.User.UID = "other"
	}

	return wire.Forbidden
}

func TestPreparedIssuerCoversReplacementActivation(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, margin := range []time.Duration{-time.Second, 0, time.Second} {
			t.Run(fmt.Sprintf("early=%v/margin=%s", early, margin), func(t *testing.T) {
				exercisePreparedIssuerHorizon(t, early, margin)
			})
		}
	}
}

func exercisePreparedIssuerHorizon(t *testing.T, early bool, margin time.Duration) {
	t.Helper()
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

	rebindSigning(&bundle, &state, material, oldID, short, true)

	bundle.Generation++
	writeSigningCredentials(t, r, bundle, state, material)
	runKeys(t, r)

	_, after, next, keys := keyState(t, r)
	if margin < 0 {
		if containsRoot(after, shortID) || next.PreparedIssuer == shortID || next.ActiveIssuer != initial.ActiveIssuer || !next.ActivateAt.Equal(now.Add(r.Config.Rotation.PrepareFor)) {
			t.Fatal("insufficient signing horizon did not restart preparation")
		}

		require.NotContains(t, keys.Keys, shortID, "stale private material retained")
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
}
