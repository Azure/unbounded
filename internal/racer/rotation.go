// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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

func validCredentialClaim(cfg Config, claim string) bool {
	return strings.HasPrefix(claim, cfg.IssuerSecretName+"/"+cfg.KeyringSecretName+"/")
}

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

func (s RotationState) nextTransition() time.Time {
	deadline := s.NextRotation
	if s.PreparedIssuer != "" {
		deadline = s.ActivateAt
	}

	for _, at := range s.Retiring {
		if at.Before(deadline) {
			deadline = at
		}
	}

	return deadline
}

func newCacheKey(cache wire.CacheID, purpose wire.KeyPurpose, state wire.KeyState, generation wire.Generation) (wire.CacheKey, error) {
	if generation == 0 {
		return wire.CacheKey{}, wire.Unavailable
	}

	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return wire.CacheKey{}, err
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return wire.CacheKey{}, err
	}
	// Reserve a versioned namespace in the otherwise opaque wire ID. A node can
	// reject reintroduced epochs using its bundle high-water mark, without keeping
	// every retired ID. The suffix distinguishes keys minted in competing CAS attempts.
	copy(id, "RKG1")
	binary.BigEndian.PutUint64(id[4:12], uint64(generation))

	return wire.NewCacheKey(wire.CacheKeyRef{Cache: cache, Purpose: purpose, ID: id}, state, material)
}

// PlanRotation owns its output. Deadlines are measured from actual transitions,
// never advanced through missed intervals after downtime. Issuer staging is done
// by reconcileKeys before this planner; publication persists private material first.
func (r *KeyringReconciler) PlanRotation(b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time) (wire.KeyringBundle, RotationState, error) {
	var creationGeneration wire.Generation
	if b.Generation < math.MaxUint64 {
		creationGeneration = b.Generation + 1
	}

	return r.planRotation(b, s, catalog, now, creationGeneration)
}

// creationGeneration is the publication that will first contain new keys. Zero
// forbids key creation when generations are exhausted, while allowing idle plans.
func (r *KeyringReconciler) planRotation(b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time, creationGeneration wire.Generation) (wire.KeyringBundle, RotationState, error) {
	// Keep wire validation and the encoded size bound at the input boundary.
	// Ownership does not require decoding the just-validated representation.
	if _, err := wire.EncodeBundle(b); err != nil {
		return wire.KeyringBundle{}, RotationState{}, err
	}

	rootsCopy := make([][]byte, len(b.PeerTrustRoots))
	for i, root := range b.PeerTrustRoots {
		rootsCopy[i] = bytes.Clone(root)
	}

	b.PeerTrustRoots = rootsCopy
	// Like DecodeBundle, normalize an empty key collection to a non-nil slice.
	keysCopy := make([]wire.CacheKey, len(b.CacheKeys))
	for i, key := range b.CacheKeys {
		keysCopy[i] = key // Includes the value-owned [32]byte material.
		keysCopy[i].Key.ID = bytes.Clone(key.Key.ID)
	}

	b.CacheKeys = keysCopy

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
				k, err := newCacheKey(cache.ID, purpose, wire.ActiveKey, creationGeneration)
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

			next, err := newCacheKey(k.Key.Cache, k.Key.Purpose, wire.PreparedKey, creationGeneration)
			if err != nil {
				return b, s, err
			}

			prepared = append(prepared, next)
		}

		b.CacheKeys = append(b.CacheKeys, prepared...)
		s.ActivateAt = now.Add(r.Config.Rotation.PrepareFor)
	}

	s.NextTransition = s.nextTransition()

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

	credentials.bundle, credentials.rotation, err = r.PlanRotation(credentials.bundle, credentials.rotation, catalog, now)
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

	b, s, err = r.planRotation(b, s, catalog, now, 1)
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

func containsRoot(b wire.KeyringBundle, id string) bool {
	for _, root := range b.PeerTrustRoots {
		if rootID(root) == id {
			return true
		}
	}

	return false
}

type credentialState struct {
	issuer   *corev1.Secret
	shared   *corev1.Secret
	bundle   wire.KeyringBundle
	rotation RotationState
	material issuerMaterial
	// Parsed once per authoritative read, never used to install candidate trust.
	signing map[string]parsedSigning
}

func readCredentials(ctx context.Context, reader client.Reader, cfg Config, claim string) (credentialState, error) {
	var (
		issuer, shared corev1.Secret
		b              wire.KeyringBundle
		s              RotationState
		material       issuerMaterial
	)

	for _, entry := range []struct {
		name   string
		secret *corev1.Secret
	}{{cfg.IssuerSecretName, &issuer}, {cfg.KeyringSecretName, &shared}} {
		if err := ctx.Err(); err != nil {
			return credentialState{}, err
		}

		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: entry.name}, entry.secret); err != nil {
			return credentialState{}, authorityReadFailure(err)
		}

		if claim == "" || entry.secret.Annotations[credentialClaim] != claim || entry.secret.DeletionTimestamp != nil || entry.secret.ResourceVersion == "" {
			return credentialState{}, wire.Unavailable
		}
	}

	var err error

	b, err = wire.DecodeBundle(bytes.NewReader(shared.Data["bundle.json"]))
	if err != nil {
		return credentialState{}, err
	}

	if b.Cluster != cfg.Cluster || json.Unmarshal(shared.Data["rotation.json"], &s) != nil || json.Unmarshal(issuer.Data["issuer.json"], &material) != nil {
		return credentialState{}, wire.Unavailable
	}

	credentials := credentialState{issuer: &issuer, shared: &shared, bundle: b, rotation: s, material: material}
	if err := credentials.validateRotation(); err != nil {
		return credentialState{}, err
	}

	return credentials, nil
}

func (c *credentialState) validateRotation() error {
	b, s, m := c.bundle, c.rotation, c.material
	if s.NextRotation.IsZero() || s.NextTransition.IsZero() || s.Retiring == nil || !containsRoot(b, s.ActiveIssuer) || (s.PreparedIssuer == "") != s.ActivateAt.IsZero() {
		return wire.Unavailable
	}

	c.signing = make(map[string]parsedSigning, len(m.Keys))
	for id, material := range m.Keys {
		if rootID(material.Certificate) != id {
			return wire.Unavailable
		}

		cert, key, err := parseSigning(material)
		if err != nil {
			return err
		}

		c.signing[id] = parsedSigning{certificate: cert, key: key}
	}

	expected := map[string]time.Time{}

	if !s.ActivateAt.IsZero() {
		if !containsRoot(b, s.PreparedIssuer) || s.PreparedIssuer == s.ActiveIssuer || !s.ActivateAt.After(s.NextRotation) {
			return wire.Unavailable
		}
	}

	for _, root := range b.PeerTrustRoots {
		id := rootID(root)

		key, ok := c.signing[id]
		if !ok || !bytes.Equal(key.certificate.Raw, root) {
			return wire.Unavailable
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
	}

	if !s.nextTransition().Equal(s.NextTransition) {
		return wire.Unavailable
	}

	if m.Pending != "" {
		if _, ok := m.Keys[m.Pending]; !ok {
			return wire.Unavailable
		}
	}

	return nil
}
