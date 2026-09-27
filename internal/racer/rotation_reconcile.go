// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func (r *KeyringReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

func credentialSecret(cfg Config, name, claim string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: name, Annotations: map[string]string{credentialClaim: claim}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
}

func (r *KeyringReconciler) reconcileKeys(ctx context.Context) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Config.Validate(); err != nil {
		return ctrl.Result{}, err
	}

	version, _, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		return ctrl.Result{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return ctrl.Result{}, authorityReadFailure(err)
	}

	catalog, err := BuildCatalog(caches.Items)
	if err != nil {
		return ctrl.Result{}, err
	}

	claim := version.Annotations[credentialClaim]
	if claim == "" {
		return r.initializeKeys(ctx, version, catalog)
	}

	if !validCredentialClaim(r.Config, claim) {
		return ctrl.Result{}, wire.Unavailable
	}

	credentials, err := readCredentials(ctx, r.APIReader, r.Config, claim)
	if err != nil {
		return ctrl.Result{}, err
	}

	catalog, err = admitCatalog(ctx, r.Config, catalog, credentials.bundle)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	credentials.discardStalePreparation(r.Config, now)

	issuerChanged, err := credentials.prepareIssuer(r.Config, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	credentials.bundle, credentials.rotation, err = PlanRotation(r.Config.Rotation, credentials.bundle, credentials.rotation, catalog, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	bundleChanged, err := credentials.encodeRotation(issuerChanged)
	if err != nil {
		return ctrl.Result{}, err
	}

	if bundleChanged {
		if err := r.publishRotation(ctx, &credentials, issuerChanged); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.pruneIssuerMaterial(ctx, &credentials); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: max(time.Second, credentials.rotation.NextTransition.Sub(now))}, nil
}

func (c *credentialState) discardStalePreparation(cfg Config, now time.Time) {
	b, s := &c.bundle, &c.rotation
	// Downtime may exhaust a staged root's useful lifetime. Cancel that unused
	// preparation and stage a fresh replacement with a full new preparation delay.
	if s.PreparedIssuer != "" {
		cert := c.signing[s.PreparedIssuer].certificate

		if now.Add(cfg.Rotation.PrepareFor + cfg.certificateLifetime()).After(cert.NotAfter) {
			roots := b.PeerTrustRoots[:0]
			for _, root := range b.PeerTrustRoots {
				if rootID(root) != s.PreparedIssuer {
					roots = append(roots, root)
				}
			}

			b.PeerTrustRoots = roots

			keys := b.CacheKeys[:0]
			for _, key := range b.CacheKeys {
				if key.State != wire.PreparedKey {
					keys = append(keys, key)
				}
			}

			b.CacheKeys = keys
			s.PreparedIssuer, s.ActivateAt, s.NextRotation = "", time.Time{}, now
		}
	}
}

func (c *credentialState) prepareIssuer(cfg Config, now time.Time) (bool, error) {
	b, s, material := &c.bundle, &c.rotation, &c.material
	issuerChanged := false

	if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		// An unreferenced pending root is a recoverable write-ahead record. Reuse
		// it after ambiguous writes instead of generating a different replacement.
		pending := material.Pending
		if pending != "" && !containsRoot(*b, pending) {
			cert := c.signing[pending].certificate

			if now.Add(cfg.Rotation.PrepareFor + cfg.certificateLifetime()).After(cert.NotAfter) {
				pending = ""
			}
		}

		if pending == "" || containsRoot(*b, pending) {
			cert, key, err := generateIssuer(now, cfg)
			if err != nil {
				return false, err
			}

			pending = rootID(cert)

			next := issuerMaterial{Pending: pending, Keys: map[string]signingMaterial{pending: {Certificate: cert, PrivateKey: key}}}
			for id, key := range material.Keys {
				next.Keys[id] = key
			}

			*material = next
			issuerChanged = true
		}

		b.PeerTrustRoots = append(b.PeerTrustRoots, material.Keys[pending].Certificate)
		s.PreparedIssuer = pending
	}

	return issuerChanged, nil
}

// encodeRotation validates the complete candidate, including its publication
// generation, before either Secret can be written.
func (c *credentialState) encodeRotation(issuerChanged bool) (bool, error) {
	encoded, err := wire.EncodeBundle(c.bundle)
	if err != nil {
		return false, err
	}

	stateBytes, err := json.Marshal(c.rotation)
	if err != nil {
		return false, err
	}

	if bytes.Equal(encoded, c.shared.Data["bundle.json"]) && bytes.Equal(stateBytes, c.shared.Data["rotation.json"]) {
		return false, nil
	}

	if c.bundle.Generation == math.MaxUint64 {
		return false, wire.Unavailable
	}

	c.bundle.Generation++

	c.shared.Data["bundle.json"], err = wire.EncodeBundle(c.bundle)
	if err != nil {
		return false, err
	}

	c.shared.Data["rotation.json"] = stateBytes

	if issuerChanged {
		c.issuer.Data["issuer.json"], err = json.Marshal(c.material)
		if err != nil {
			return false, err
		}
	}

	return true, nil
}

func (r *KeyringReconciler) publishRotation(ctx context.Context, c *credentialState, issuerChanged bool) error {
	// Private write-ahead material must be durable before publishing its root.
	if issuerChanged {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := r.Update(ctx, c.issuer); err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return r.Update(ctx, c.shared)
}

func (r *KeyringReconciler) pruneIssuerMaterial(ctx context.Context, c *credentialState) error {
	// Remove private material only after the common bundle no longer references
	// it. A crash here leaves harmless extra private keys, never dangling trust.
	next, material, issuer := c.bundle, c.material, c.issuer
	clean := issuerMaterial{Pending: material.Pending, Keys: map[string]signingMaterial{}}

	for _, root := range next.PeerTrustRoots {
		id := rootID(root)
		clean.Keys[id] = material.Keys[id]
	}

	if !containsRoot(next, clean.Pending) {
		clean.Pending = ""
	}

	if !reflect.DeepEqual(clean, material) {
		var err error

		issuer.Data["issuer.json"], err = json.Marshal(clean)
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if err := r.Update(ctx, issuer); err != nil {
			return err
		}
	}

	return nil
}

func (r *KeyringReconciler) initializeKeys(ctx context.Context, version *corev1.ConfigMap, catalog []wire.CacheDefinition) (ctrl.Result, error) {
	for _, name := range []string{r.Config.IssuerSecretName, r.Config.KeyringSecretName} {
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			if err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, wire.Unavailable
		}
	}

	now := r.now()

	cert, key, err := generateIssuer(now, r.Config)
	if err != nil {
		return ctrl.Result{}, err
	}

	id := rootID(cert)
	b := wire.KeyringBundle{SchemaVersion: wire.SchemaVersion, Cluster: r.Config.Cluster, Generation: 1, PeerTrustRoots: [][]byte{cert}}
	s := RotationState{ActiveIssuer: id, NextRotation: now.Add(r.Config.Rotation.Interval), Retiring: map[string]time.Time{}}

	catalog, err = admitCatalog(ctx, r.Config, catalog, b)
	if err != nil {
		return ctrl.Result{}, err
	}

	b, s, err = planRotation(r.Config.Rotation, b, s, catalog, now, 1)
	if err != nil {
		return ctrl.Result{}, err
	}

	encoded, err := wire.EncodeBundle(b)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The permanent claim is on the already-required version object. Topology
	// preserves annotations with its CAS. Missing Secrets after this claim never
	// authorize Create on recovery, even when the first create response was lost.
	claim := fmt.Sprintf("%s/%s/%s", r.Config.IssuerSecretName, r.Config.KeyringSecretName, id)
	version.Annotations[credentialClaim] = claim

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Update(ctx, version); err != nil {
		return ctrl.Result{}, err
	}

	issuer := credentialSecret(r.Config, r.Config.IssuerSecretName, claim)

	issuer.Data["issuer.json"], err = json.Marshal(issuerMaterial{Keys: map[string]signingMaterial{id: {Certificate: cert, PrivateKey: key}}})
	if err != nil {
		return ctrl.Result{}, err
	}

	shared := credentialSecret(r.Config, r.Config.KeyringSecretName, claim)
	shared.Data["bundle.json"] = encoded

	shared.Data["rotation.json"], err = json.Marshal(s)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, secret := range []*corev1.Secret{issuer, shared} {
		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, secret); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: r.Config.Rotation.Interval}, nil
}
