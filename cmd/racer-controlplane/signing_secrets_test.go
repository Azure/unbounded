// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func signingTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func signingTestSecret(t *testing.T, name string, seed byte) *corev1.Secret {
	t.Helper()
	key := testSigner(t, seed)
	ring := &signingRing{Version: 1, Generation: 1, Active: ringKey{Seed: hex.EncodeToString(key.key.Seed()), Public: hex.EncodeToString(key.key[32:])}, ActivatedAt: time.Now().UTC()}

	data, err := ring.data(name == peerSigningSecret)
	if err != nil {
		t.Fatal(err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func TestManagedSigningCreationAndRestart(t *testing.T) {
	ctx := context.Background()
	c := signingTestClient(t)

	key, err := ensureManagedSigning(ctx, c, "state")
	if err != nil {
		t.Fatal(err)
	}

	var (
		seeds  [][]byte
		before corev1.SecretList
	)

	if err := c.List(ctx, &before); err != nil || len(before.Items) != 2 {
		t.Fatalf("expected two Secrets: %v", err)
	}

	for _, name := range []string{configSigningSecret, peerSigningSecret} {
		var secret corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: "state", Name: name}, &secret); err != nil {
			t.Fatal(err)
		}

		ring, err := readSigningRing(&secret)
		if err != nil {
			t.Fatal(err)
		}

		seed, _ := hex.DecodeString(ring.Active.Seed)

		public, _ := hex.DecodeString(ring.Active.Public)
		if secret.Type != corev1.SecretTypeOpaque || len(seed) != 32 || len(public) != 32 ||
			!bytes.Equal(public, ed25519.NewKeyFromSeed(seed)[32:]) || bytes.Equal(seed, make([]byte, 32)) {
			t.Fatalf("invalid generated pair in %s", name)
		}

		seeds = append(seeds, seed)
		if name == configSigningSecret && !bytes.Equal(key.key.Seed(), seed) {
			t.Fatal("initial signer differs from persisted config key")
		}
	}

	if bytes.Equal(seeds[0], seeds[1]) {
		t.Fatal("config and peer keys are identical")
	}
	// Restart must issue only reads and preserve metadata and material exactly.
	readOnly := &signingTestAPI{Client: c, create: func(context.Context, client.Object, ...client.CreateOption) error {
		t.Fatal("restart attempted to create existing Secret")
		return nil
	}}

	next, err := ensureManagedSigning(ctx, readOnly, "state")
	if err != nil || next.id != key.id {
		t.Fatalf("restart did not reuse persisted signer: %v", err)
	}

	var after corev1.SecretList
	if err := c.List(ctx, &after); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restart changed Secrets: %v", err)
	}

	other, err := ensureManagedSigning(ctx, c, "other-state")
	if err != nil || other.id == key.id {
		t.Fatalf("namespace isolation failed: %v", err)
	}
}

func TestManagedSigningMissingSecretStartup(t *testing.T) {
	for _, name := range []string{configSigningSecret, peerSigningSecret} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			existing := signingTestSecret(t, name, 7)
			c := signingTestClient(t, existing)

			var before corev1.Secret
			if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &before); err != nil {
				t.Fatal(err)
			}

			key, err := ensureManagedSigning(ctx, c, "state")
			if err != nil {
				t.Fatal(err)
			}

			var after corev1.Secret
			if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &after); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("provisioning missing Secret modified existing material: %v", err)
			}
			// Startup provisions absence, including a deletion preceding restart.
			if err := c.Delete(ctx, &after); err != nil {
				t.Fatal(err)
			}

			restarted, err := ensureManagedSigning(ctx, c, "state")
			if err != nil {
				t.Fatal(err)
			}

			if (restarted.id == key.id) != (name == peerSigningSecret) {
				t.Fatal("restart changed the wrong signing identity")
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(existing), &after); err != nil || bytes.Equal(before.Data[ringFile], after.Data[ringFile]) {
				t.Fatalf("restart did not provision missing Secret: %v", err)
			}
		})
	}
}

type signingTestAPI struct {
	client.Client
	get    func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
	create func(context.Context, client.Object, ...client.CreateOption) error
	update func(context.Context, client.Object, ...client.UpdateOption) error
}

func (c *signingTestAPI) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if c.update != nil {
		return c.update(ctx, object, opts...)
	}

	return c.Client.Update(ctx, object, opts...)
}

func (c *signingTestAPI) Get(ctx context.Context, name client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if c.get != nil {
		return c.get(ctx, name, object, opts...)
	}

	return c.Client.Get(ctx, name, object, opts...)
}

func (c *signingTestAPI) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if c.create != nil {
		return c.create(ctx, object, opts...)
	}

	return c.Client.Create(ctx, object, opts...)
}

func TestManagedSigningConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	c := signingTestClient(t)

	var arrived, done sync.WaitGroup
	arrived.Add(2)
	// Both replicas observe config NotFound before either can create it.
	racing := &signingTestAPI{Client: c, get: func(ctx context.Context, name client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, name, object, opts...)
		if name.Name == configSigningSecret && apierrors.IsNotFound(err) {
			arrived.Done()
			arrived.Wait()
		}

		return err
	}}
	keys := make([]*signer, 2)
	errs := make([]error, 2)

	for i := range keys {
		done.Add(1)

		go func(i int) {
			defer done.Done()

			keys[i], errs[i] = ensureManagedSigning(ctx, racing, "state")
		}(i)
	}

	done.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("replica startup failed: %v", errs)
	}

	if keys[0].id != keys[1].id {
		t.Fatal("replicas installed different signers")
	}

	stored, err := ensureManagedSigning(ctx, c, "state")
	if err != nil || stored.id != keys[0].id {
		t.Fatalf("replicas did not install winning key: %v", err)
	}
}

func TestManagedSigningInvalidMaterial(t *testing.T) {
	for _, name := range []string{configSigningSecret, peerSigningSecret} {
		for _, mutation := range []struct {
			name string
			edit func(*corev1.Secret)
		}{
			{"missing-data", func(s *corev1.Secret) { s.Data = nil }},
			{"missing-ring", func(s *corev1.Secret) { delete(s.Data, ringFile) }},
			{"truncated-ring", func(s *corev1.Secret) { s.Data[ringFile] = s.Data[ringFile][:31] }},
			{"trailing-ring", func(s *corev1.Secret) { s.Data[ringFile] = append(s.Data[ringFile], 0) }},
			{"missing-bundle", func(s *corev1.Secret) { delete(s.Data, bundleFile) }},
			{"mismatched-bundle", func(s *corev1.Secret) { s.Data[bundleFile][0] ^= 1 }},
			{"terminating", func(s *corev1.Secret) {
				now := metav1.Now()
				s.DeletionTimestamp = &now
				s.Finalizers = []string{"test"}
			}},
		} {
			t.Run(name+"/"+mutation.name, func(t *testing.T) {
				config, peer := signingTestSecret(t, configSigningSecret, 7), signingTestSecret(t, peerSigningSecret, 8)

				bad := config
				if name == peerSigningSecret {
					bad = peer
				}

				mutation.edit(bad)

				c := signingTestClient(t, config, peer)

				var before, after corev1.SecretList
				if err := c.List(context.Background(), &before); err != nil {
					t.Fatal(err)
				}

				if key, err := ensureManagedSigning(context.Background(), c, "state"); err == nil || key != nil {
					t.Fatal("invalid existing material did not fail closed")
				}

				if err := c.List(context.Background(), &after); err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("invalid material was modified: %v", err)
				}
			})
		}
	}
}

func TestManagedSigningCreateRaceWinner(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[valid], func(t *testing.T) {
			c := signingTestClient(t)

			winner := signingTestSecret(t, configSigningSecret, 9)
			if !valid {
				winner.Data[bundleFile][0] ^= 1
			}

			calls := 0
			racing := &signingTestAPI{Client: c, create: func(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
				calls++

				if err := c.Create(ctx, winner); err != nil {
					return err
				}

				return c.Create(ctx, object, opts...)
			}}

			key, err := ensureSigningSecret(context.Background(), racing, client.ObjectKeyFromObject(winner))
			if (err == nil) != valid || calls != 1 {
				t.Fatalf("unexpected race outcome: calls=%d err=%v", calls, err)
			}

			if valid && key.id != testSigner(t, 9).id {
				t.Fatal("loser installed its own generated key")
			}

			if !valid && key != nil {
				t.Fatal("invalid winner installed")
			}
		})
	}
}

func TestManagedSigningAPIErrors(t *testing.T) {
	for _, stage := range []string{"get", "create", "race-reread"} {
		for _, failure := range []error{
			apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, configSigningSecret, errors.New("denied")),
			apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, configSigningSecret),
			apierrors.NewServiceUnavailable("offline"),
		} {
			t.Run(stage+"/"+failure.Error(), func(t *testing.T) {
				c := signingTestClient(t)
				gets, creates := 0, 0
				broken := &signingTestAPI{Client: c}
				broken.get = func(ctx context.Context, name client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					gets++
					if stage == "get" || stage == "race-reread" && gets > 1 {
						return failure
					}

					return c.Get(ctx, name, object, opts...)
				}
				broken.create = func(context.Context, client.Object, ...client.CreateOption) error {
					creates++

					if stage == "race-reread" {
						return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, configSigningSecret)
					}

					return failure
				}

				key, err := ensureManagedSigning(context.Background(), broken, "state")
				if key != nil || !errors.Is(err, failure) {
					t.Fatalf("API error not propagated: %v", err)
				}

				if stage == "get" && !apierrors.IsNotFound(failure) && creates != 0 {
					t.Fatal("created after failed read")
				}

				if stage == "race-reread" && (gets != 2 || creates != 1) {
					t.Fatal("failed race reread retried creation")
				}
			})
		}
	}
}

func TestManagedSigningReload(t *testing.T) {
	ctx := context.Background()
	c := signingTestClient(t)

	key, err := ensureManagedSigning(ctx, c, "state")
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{signer: key}
	snapshot := installTestGeneration(t, s, testGeneration(8, 2))
	r := &signingSecretReconciler{client: c, server: s, namespace: "state"}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "state", Name: configSigningSecret}}
	reconcile := func(wantError bool) {
		t.Helper()

		if _, err := r.Reconcile(ctx, request); (err != nil) != wantError {
			t.Fatalf("reload error=%v, want error=%t", err, wantError)
		}
	}
	response := get(handler(s), target(snapshot), "", "")

	reconcile(false) // Initial informer add of the same key is a no-op.

	if got := get(handler(s), target(snapshot), "", ""); !bytes.Equal(got.Body.Bytes(), response.Body.Bytes()) {
		t.Fatal("same key changed publication")
	}

	var secret corev1.Secret
	if err := c.Get(ctx, request.NamespacedName, &secret); err != nil {
		t.Fatal(err)
	}

	next := testSigner(t, 42)
	ring, _ := readSigningRing(&secret)
	ring.Generation++
	ring.Previous = ring.Active.Public
	ring.Active = ringKey{Seed: hex.EncodeToString(next.key.Seed()), Public: hex.EncodeToString(next.key[32:])}

	secret.Data, _ = ring.data(false)
	if err := c.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}

	reconcile(false)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), next, snapshot)
	// Malformed public/seed pairs and deleted Secrets retain the last valid key.
	secret.Data[bundleFile][0] ^= 1
	if err := c.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}

	reconcile(true)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), next, snapshot)

	if err := c.Delete(ctx, &secret); err != nil {
		t.Fatal(err)
	}

	reconcile(true)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), next, snapshot)

	if err := c.Get(ctx, request.NamespacedName, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("reload recreated deleted Secret")
	}
	// Replacement must preserve monotonic generation after deletion.
	replacement := signingTestSecret(t, configSigningSecret, 43)
	replacementRing, _ := readSigningRing(replacement)
	replacementRing.Generation = ring.Generation + 1

	replacement.Data, _ = replacementRing.data(false)
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	reconcile(false)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), testSigner(t, 43), snapshot)
	// Peer updates are validated, but never become the config signing identity.
	request.Name = peerSigningSecret

	reconcile(false)

	if err := c.Get(ctx, request.NamespacedName, &secret); err != nil {
		t.Fatal(err)
	}

	secret.Data[ringFile] = nil
	if err := c.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}

	reconcile(true)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), testSigner(t, 43), snapshot)

	request.Name = "unrelated"

	reconcile(false)

	request.Name, request.Namespace = configSigningSecret, "other-state"

	reconcile(false)

	request.Namespace = "state"
	r.client = &signingTestAPI{Client: c, get: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewServiceUnavailable("offline")
	}}

	reconcile(true)
	checkSigned(t, get(handler(s), target(snapshot), "", ""), testSigner(t, 43), snapshot)
}

func TestRotationCommitRecovery(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			ctx := context.Background()
			s := signingTestSecret(t, configSigningSecret, 7)
			ring, _ := readSigningRing(s)
			now := ring.ActivatedAt.Add(24 * time.Hour)
			c := signingTestClient(t, s)
			broken := &signingTestAPI{Client: c, update: func(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
				if committed {
					if err := c.Update(ctx, o, opts...); err != nil {
						return err
					}
				}

				return apierrors.NewTimeoutError("ambiguous write", 1)
			}}
			r := &signingRotationReconciler{client: broken, namespace: "state", policy: rotationPolicy{24 * time.Hour, 10 * time.Minute}, now: func() time.Time { return now }}

			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)}
			if _, err := r.Reconcile(ctx, req); err == nil {
				t.Fatal("lost write response ignored")
			}

			now = now.Add(time.Hour)

			r.client = c // new leader reads durable state
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			if !committed {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}

			if err := c.Get(ctx, req.NamespacedName, s); err != nil {
				t.Fatal(err)
			}

			after, err := readSigningRing(s)
			if err != nil || after.Active != ring.Active || !after.ActivateAfter.Equal(now.Add(10*time.Minute)) {
				t.Fatalf("shortened grace: %v", err)
			}
			// Competing writer takes the resourceVersion first. Our stale candidate
			// must fail CAS without installing its active key.
			now = now.Add(10 * time.Minute)

			r.client = &signingTestAPI{Client: c, update: func(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
				var current corev1.Secret
				if err := c.Get(ctx, req.NamespacedName, &current); err != nil {
					return err
				}

				current.Annotations = map[string]string{"writer": "other"}
				if err := c.Update(ctx, &current); err != nil {
					return err
				}

				return c.Update(ctx, o, opts...)
			}}
			if _, err := r.Reconcile(ctx, req); !apierrors.IsConflict(err) {
				t.Fatalf("expected CAS conflict: %v", err)
			}

			r.client = c
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			if err := c.Get(ctx, req.NamespacedName, s); err != nil {
				t.Fatal(err)
			}

			active, _ := readSigningRing(s)
			if active.Active == ring.Active || active.Previous != ring.Active.Public {
				t.Fatal("activation failed")
			}
		})
	}
}
