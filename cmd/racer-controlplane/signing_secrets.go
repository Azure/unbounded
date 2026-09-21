// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	configSigningSecret = "racer-config-signing"
	peerSigningSecret   = "racer-peer-signing"
)

// Provision only at startup, using an uncached client before the manager starts.
// Existing material is never repaired or replaced. Deletion during runtime keeps
// the last valid signer; restart with a missing Secret provisions a new key, as
// absence cannot be distinguished from first installation without durable state.
func ensureManagedSigning(ctx context.Context, c client.Client, namespace string) (*signer, error) {
	var config *signer

	for _, name := range []string{configSigningSecret, peerSigningSecret} {
		key, err := ensureSigningSecret(ctx, c, types.NamespacedName{Namespace: namespace, Name: name})
		if err != nil {
			return nil, fmt.Errorf("signing Secret %s/%s: %w", namespace, name, err)
		}

		if name == configSigningSecret {
			config = key
		} else {
			clear(key.key)
		}
	}

	return config, nil
}

func ensureSigningSecret(ctx context.Context, c client.Client, name types.NamespacedName) (*signer, error) {
	secret := &corev1.Secret{}

	err := c.Get(ctx, name, secret)
	if apierrors.IsNotFound(err) {
		ring, generateErr := newSigningRing(time.Now())
		if generateErr != nil {
			return nil, generateErr
		}

		data, generateErr := ring.data(name.Name == peerSigningSecret)
		if generateErr != nil {
			return nil, generateErr
		}

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: name.Namespace, Name: name.Name},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}

		err = c.Create(ctx, secret)
		if apierrors.IsAlreadyExists(err) {
			// Another replica won. Only its persisted key may be installed.
			secret = &corev1.Secret{}
			err = c.Get(ctx, name, secret)
		}
	}

	if err != nil {
		return nil, err
	}

	return signerFromSecret(secret)
}

func signerFromSecret(secret *corev1.Secret) (*signer, error) {
	ring, err := readSigningRing(secret)
	if err != nil {
		return nil, err
	}

	return ring.Active.signer()
}

// Reload is read-only: delete events and malformed updates must not silently
// rotate identity. Returning errors retains the last valid signer and retries
// transient API failures through controller-runtime's work queue.
type signingSecretReconciler struct {
	client    client.Reader
	server    *Server
	namespace string
	mu        sync.Mutex
	seen      map[string]*corev1.Secret
}

func (r *signingSecretReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if request.Namespace != r.namespace || (request.Name != configSigningSecret && request.Name != peerSigningSecret) {
		return ctrl.Result{}, nil
	}

	var secret corev1.Secret

	err := r.client.Get(ctx, request.NamespacedName, &secret)
	if err == nil {
		err = r.observe(&secret)
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("signing Secret %s: retaining previous key: %w", request.NamespacedName, err)
	}

	return ctrl.Result{}, nil
}

func (r *signingSecretReconciler) observe(secret *corev1.Secret) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ring, err := readSigningRing(secret)
	if err != nil {
		return err
	}

	if previous := r.seen[secret.Name]; previous != nil {
		old, err := readSigningRing(previous)
		if err != nil {
			return err
		}

		if ring.Generation < old.Generation || ring.Generation == old.Generation && !bytes.Equal(secret.Data[ringFile], previous.Data[ringFile]) {
			return fmt.Errorf("signing ring rollback or equivocation")
		}
	}

	if secret.Name == configSigningSecret {
		key, err := ring.Active.signer()
		if err != nil {
			return err
		}

		if err := r.server.rotate(key); err != nil {
			return err
		}
	}

	if r.seen == nil {
		r.seen = make(map[string]*corev1.Secret)
	}

	r.seen[secret.Name] = secret.DeepCopy()

	return nil
}

func setupSigningController(manager ctrl.Manager, observer *signingSecretReconciler, writer client.Client, policy rotationPolicy) error {
	leaderOnly := false

	filter := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == observer.namespace && (o.GetName() == configSigningSecret || o.GetName() == peerSigningSecret)
	})
	if err := ctrl.NewControllerManagedBy(manager).Named("signing-keys").
		For(&corev1.Secret{}).
		WithEventFilter(filter).
		WithOptions(controller.Options{NeedLeaderElection: &leaderOnly}).
		Complete(observer); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(manager).Named("signing-rotation").
		For(&corev1.Secret{}).WithEventFilter(filter).
		Complete(&signingRotationReconciler{client: writer, namespace: observer.namespace, policy: policy, now: time.Now})
}

type signingRotationReconciler struct {
	client    client.Client // uncached, including all retry reads
	namespace string
	policy    rotationPolicy
	now       func() time.Time
}

func (r *signingRotationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if request.Namespace != r.namespace || (request.Name != configSigningSecret && request.Name != peerSigningSecret) {
		return ctrl.Result{}, nil
	}

	var secret corev1.Secret
	if err := r.client.Get(ctx, request.NamespacedName, &secret); err != nil {
		return ctrl.Result{}, err
	}

	ring, err := readSigningRing(&secret)
	if err != nil {
		return ctrl.Result{}, err
	}

	changed, wait, err := ring.advance(r.now(), r.policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !changed {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	secret.Data, err = ring.data(request.Name == peerSigningSecret)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A failed/ambiguous write discards the candidate. The next reconcile reads
	// committed state and cannot treat an unconfirmed publication as warmed up.
	if err := r.client.Update(ctx, &secret); err != nil {
		return ctrl.Result{}, err
	}

	ctrl.LoggerFrom(ctx).Info("committed signing rotation", "secret", request.Name, "generation", ring.Generation, "active", ring.Active.Public, "activateAfter", ring.ActivateAfter)

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}
