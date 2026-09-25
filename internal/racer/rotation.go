// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

const credentialClaim = "racer.unbounded-cloud.io/credentials"

func rootID(der []byte) string { sum := sha256.Sum256(der); return hex.EncodeToString(sum[:]) }
func keyID(k wire.CacheKey) string {
	return string(k.Key.Cache) + "/" + string(k.Key.Purpose) + "/" + hex.EncodeToString(k.Key.ID)
}

func keyScope(k wire.CacheKey) string { return string(k.Key.Cache) + "/" + string(k.Key.Purpose) }

func (r *KeyringReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

func newCacheKey(cache wire.CacheID, purpose wire.KeyPurpose, state wire.KeyState) (wire.CacheKey, error) {
	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return wire.CacheKey{}, err
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return wire.CacheKey{}, err
	}

	return wire.NewCacheKey(wire.CacheKeyRef{Cache: cache, Purpose: purpose, ID: id}, state, material)
}

// PlanRotation owns its output. Deadlines are measured from actual transitions,
// never advanced through missed intervals after downtime. Issuer staging is done
// by reconcileKeys before this planner, with private material persisted first.
func (r *KeyringReconciler) PlanRotation(b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time) (wire.KeyringBundle, RotationState, error) {
	encoded, err := wire.EncodeBundle(b)
	if err != nil {
		return wire.KeyringBundle{}, RotationState{}, err
	}

	b, err = wire.DecodeBundle(bytes.NewReader(encoded))
	if err != nil {
		return wire.KeyringBundle{}, RotationState{}, err
	}

	retiring := make(map[string]time.Time, len(s.Retiring))
	for id, deadline := range s.Retiring {
		retiring[id] = deadline
	}

	s.Retiring = retiring

	wanted := map[wire.CacheID]bool{}
	for _, cache := range catalog {
		if !wire.ValidUUID(string(cache.ID)) || wanted[cache.ID] {
			return b, s, wire.InvalidRequest
		}

		wanted[cache.ID] = true
	}

	keys := b.CacheKeys[:0]
	for _, k := range b.CacheKeys {
		deadline, retiring := s.Retiring[keyID(k)]
		if !wanted[k.Key.Cache] || retiring && !now.Before(deadline) {
			delete(s.Retiring, keyID(k))
			continue
		}

		keys = append(keys, k)
	}

	b.CacheKeys = keys

	roots := b.PeerTrustRoots[:0]
	for _, root := range b.PeerTrustRoots {
		id := rootID(root)
		if deadline, ok := s.Retiring[id]; ok && !now.Before(deadline) {
			delete(s.Retiring, id)
			continue
		}

		roots = append(roots, root)
	}

	b.PeerTrustRoots = roots

	present := map[string]bool{}
	for _, key := range b.CacheKeys {
		present[keyScope(key)] = true
	}

	for _, cache := range catalog {
		for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
			if !present[string(cache.ID)+"/"+string(purpose)] {
				k, err := newCacheKey(cache.ID, purpose, wire.ActiveKey)
				if err != nil {
					return b, s, err
				}

				b.CacheKeys = append(b.CacheKeys, k)
			}
		}
	}

	if !s.ActivateAt.IsZero() && !now.Before(s.ActivateAt) {
		prepared := map[string]bool{}

		for _, key := range b.CacheKeys {
			if key.State == wire.PreparedKey {
				prepared[keyScope(key)] = true
			}
		}

		for i := range b.CacheKeys {
			k := &b.CacheKeys[i]
			// A cache added during preparation can have only its initial active key.
			if k.State == wire.ActiveKey && prepared[keyScope(*k)] {
				k.State = wire.RetiringKey
				s.Retiring[keyID(*k)] = now.Add(r.Config.Rotation.RetainFor)
			}
		}

		for i := range b.CacheKeys {
			if b.CacheKeys[i].State == wire.PreparedKey {
				b.CacheKeys[i].State = wire.ActiveKey
			}
		}

		s.Retiring[s.ActiveIssuer] = now.Add(r.Config.Rotation.RetainFor)
		s.ActiveIssuer, s.PreparedIssuer = s.PreparedIssuer, ""
		s.ActivateAt = time.Time{}
		s.NextRotation = now.Add(r.Config.Rotation.Interval)
	} else if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		if s.PreparedIssuer == "" {
			return b, s, wire.Unavailable
		}

		var prepared []wire.CacheKey

		for _, k := range b.CacheKeys {
			if k.State != wire.ActiveKey {
				continue
			}

			next, err := newCacheKey(k.Key.Cache, k.Key.Purpose, wire.PreparedKey)
			if err != nil {
				return b, s, err
			}

			prepared = append(prepared, next)
		}

		b.CacheKeys = append(b.CacheKeys, prepared...)
		s.ActivateAt = now.Add(r.Config.Rotation.PrepareFor)
	}

	s.NextTransition = s.NextRotation
	if !s.ActivateAt.IsZero() {
		s.NextTransition = s.ActivateAt
	}

	for _, deadline := range s.Retiring {
		if deadline.Before(s.NextTransition) {
			s.NextTransition = deadline
		}
	}

	if _, err := wire.EncodeBundle(b); err != nil {
		return b, s, err
	}

	return b, s, nil
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

	topology := &TopologyReconciler{Client: r.Client, APIReader: r.APIReader, Config: r.Config}

	version, _, err := topology.readVersion(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return ctrl.Result{}, err
	}

	catalog, err := BuildCatalog(caches.Items)
	if err != nil {
		return ctrl.Result{}, err
	}

	claim := version.Annotations[credentialClaim]
	if claim == "" {
		return r.initializeKeys(ctx, version, catalog)
	}

	if !strings.HasPrefix(claim, r.Config.IssuerSecretName+"/"+r.Config.KeyringSecretName+"/") {
		return ctrl.Result{}, wire.Unavailable
	}

	issuer, shared, b, s, material, err := readCredentials(ctx, r.APIReader, r.Config, claim)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	// Downtime may exhaust a staged root's useful lifetime. Cancel that unused
	// preparation and stage a fresh replacement with a full new preparation delay.
	if s.PreparedIssuer != "" {
		cert, _, err := parseSigning(material.Keys[s.PreparedIssuer])
		if err != nil {
			return ctrl.Result{}, err
		}

		if now.Add(r.Config.Rotation.PrepareFor + wire.CertificateLifetime).After(cert.NotAfter) {
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

	issuerChanged := false

	if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		// An unreferenced pending root is a recoverable write-ahead record. Reuse
		// it after ambiguous writes instead of generating a different replacement.
		pending := material.Pending
		if pending != "" && !containsRoot(b, pending) {
			cert, _, err := parseSigning(material.Keys[pending])
			if err != nil {
				return ctrl.Result{}, err
			}

			if now.Add(r.Config.Rotation.PrepareFor + wire.CertificateLifetime).After(cert.NotAfter) {
				pending = ""
			}
		}

		if pending == "" || containsRoot(b, pending) {
			cert, key, err := generateIssuer(now, r.Config)
			if err != nil {
				return ctrl.Result{}, err
			}

			pending = rootID(cert)

			next := issuerMaterial{Pending: pending, Keys: map[string]signingMaterial{pending: {Certificate: cert, PrivateKey: key}}}
			for id, key := range material.Keys {
				next.Keys[id] = key
			}

			material = next

			issuer.Data["issuer.json"], err = json.Marshal(material)
			if err != nil {
				return ctrl.Result{}, err
			}

			issuerChanged = true
		}

		b.PeerTrustRoots = append(b.PeerTrustRoots, material.Keys[pending].Certificate)
		s.PreparedIssuer = pending
	}

	next, state, err := r.PlanRotation(b, s, catalog, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	encoded, err := wire.EncodeBundle(next)
	if err != nil {
		return ctrl.Result{}, err
	}

	stateBytes, err := json.Marshal(state)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !bytes.Equal(encoded, shared.Data["bundle.json"]) || !bytes.Equal(stateBytes, shared.Data["rotation.json"]) {
		if next.Generation == math.MaxUint64 {
			return ctrl.Result{}, wire.Unavailable
		}

		next.Generation++

		shared.Data["bundle.json"], err = wire.EncodeBundle(next)
		if err != nil {
			return ctrl.Result{}, err
		}

		shared.Data["rotation.json"] = stateBytes

		if issuerChanged {
			if err := ctx.Err(); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Update(ctx, issuer); err != nil {
				return ctrl.Result{}, err
			}
		}

		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, shared); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Remove private material only after the common bundle no longer references
	// it. A crash here leaves harmless extra private keys, never dangling trust.
	clean := issuerMaterial{Pending: material.Pending, Keys: map[string]signingMaterial{}}

	for _, root := range next.PeerTrustRoots {
		id := rootID(root)
		clean.Keys[id] = material.Keys[id]
	}

	if !containsRoot(next, clean.Pending) {
		clean.Pending = ""
	}

	if !reflect.DeepEqual(clean, material) {
		issuer.Data["issuer.json"], err = json.Marshal(clean)
		if err != nil {
			return ctrl.Result{}, err
		}

		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, issuer); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Validate the committed pair again, including signing lifetime. Never mark
	// ready based on an uncommitted candidate or a stale informer read.
	if _, _, _, _, _, err := readCredentials(ctx, r.APIReader, r.Config, claim); err != nil {
		return ctrl.Result{}, err
	}

	if _, err := loadSigning(ctx, r.APIReader, r.Config, now); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: max(time.Second, state.NextTransition.Sub(now))}, nil
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

	b, s, err = r.PlanRotation(b, s, catalog, now)
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

	if _, err := loadSigning(ctx, r.APIReader, r.Config, now); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.Config.Rotation.Interval}, nil
}

func containsRoot(b wire.KeyringBundle, id string) bool {
	for _, root := range b.PeerTrustRoots {
		if rootID(root) == id {
			return true
		}
	}

	return false
}

func readCredentials(ctx context.Context, reader client.Reader, cfg Config, claim string) (*corev1.Secret, *corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial, error) {
	var (
		issuer, shared corev1.Secret
		b              wire.KeyringBundle
		s              RotationState
		material       issuerMaterial
	)

	fail := func(err error) (*corev1.Secret, *corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial, error) {
		return nil, nil, b, s, issuerMaterial{}, err
	}
	for _, entry := range []struct {
		name   string
		secret *corev1.Secret
	}{{cfg.IssuerSecretName, &issuer}, {cfg.KeyringSecretName, &shared}} {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}

		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: entry.name}, entry.secret); err != nil {
			return fail(err)
		}

		if claim == "" || entry.secret.Annotations[credentialClaim] != claim || entry.secret.DeletionTimestamp != nil || entry.secret.ResourceVersion == "" {
			return fail(wire.Unavailable)
		}
	}

	var err error

	b, err = wire.DecodeBundle(bytes.NewReader(shared.Data["bundle.json"]))
	if err != nil {
		return fail(err)
	}

	if b.Cluster != cfg.Cluster || json.Unmarshal(shared.Data["rotation.json"], &s) != nil || json.Unmarshal(issuer.Data["issuer.json"], &material) != nil {
		return fail(wire.Unavailable)
	}

	if err := validateRotation(b, s, material); err != nil {
		return fail(err)
	}

	return &issuer, &shared, b, s, material, nil
}

func validateRotation(b wire.KeyringBundle, s RotationState, m issuerMaterial) error {
	if s.NextRotation.IsZero() || s.NextTransition.IsZero() || s.Retiring == nil || !containsRoot(b, s.ActiveIssuer) || (s.PreparedIssuer == "") != s.ActivateAt.IsZero() {
		return wire.Unavailable
	}

	expected := map[string]time.Time{}

	deadline := s.NextRotation
	if !s.ActivateAt.IsZero() {
		if !containsRoot(b, s.PreparedIssuer) || s.PreparedIssuer == s.ActiveIssuer || !s.ActivateAt.After(s.NextRotation) {
			return wire.Unavailable
		}

		deadline = s.ActivateAt
	}

	for _, root := range b.PeerTrustRoots {
		id := rootID(root)

		key, ok := m.Keys[id]
		if !ok || !bytes.Equal(key.Certificate, root) {
			return wire.Unavailable
		}

		if _, _, err := parseSigning(key); err != nil {
			return err
		}

		if id != s.ActiveIssuer && id != s.PreparedIssuer {
			expected[id] = s.Retiring[id]
		}
	}

	prepared := map[string]bool{}

	for _, key := range b.CacheKeys {
		if key.State == wire.RetiringKey {
			expected[keyID(key)] = s.Retiring[keyID(key)]
		}

		if key.State == wire.PreparedKey {
			scope := string(key.Key.Cache) + "/" + string(key.Key.Purpose)
			if s.ActivateAt.IsZero() || prepared[scope] {
				return wire.Unavailable
			}

			prepared[scope] = true
		}
	}

	if !reflect.DeepEqual(expected, s.Retiring) {
		return wire.Unavailable
	}

	for _, at := range expected {
		if at.IsZero() {
			return wire.Unavailable
		}

		if at.Before(deadline) {
			deadline = at
		}
	}

	if !deadline.Equal(s.NextTransition) {
		return wire.Unavailable
	}

	if m.Pending != "" {
		if _, ok := m.Keys[m.Pending]; !ok {
			return wire.Unavailable
		}
	}

	for id, key := range m.Keys {
		if rootID(key.Certificate) != id {
			return wire.Unavailable
		}

		if _, _, err := parseSigning(key); err != nil {
			return err
		}
	}

	return nil
}
