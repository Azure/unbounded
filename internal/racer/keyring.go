// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type KeyringReconciler struct {
	client.Client
	APIReader   client.Reader
	Config      Config
	Trust       *Trust
	Now         func() time.Time
	CatalogGate *CatalogGate
}

// Reconcile creates/rotates issuer and cache keys through ordinary Secret CAS,
// stages trust before using a new issuer, and returns RequeueAfter for deadlines.
// Enforce projected size bounds including overlapping keys before committing.
func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if r.CatalogGate != nil {
		if err := r.CatalogGate.Acquire(ctx); err != nil {
			return ctrl.Result{}, reconcile.TerminalError(err)
		}
		defer r.CatalogGate.Release()
	}

	result, err := r.reconcileKeys(ctx)

	// Refresh committed trust while the catalog gate is still held. Failed gate
	// admission must not reach this completion path or withdraw accepted trust.
	// Install only after authoritative validation of the committed credentials,
	// including installation binding, rotation consistency, and signing lifetime.
	if err == nil {
		var state signingState

		state, err = loadSigning(ctx, r.APIReader, r.Config, r.now())
		if err == nil && r.Trust != nil {
			err = r.Trust.install(ctx, state.roots, state.bundle)
		}
	}

	// Cancellation after admission overrides even an unavailable authority read.
	if ctx.Err() != nil {
		err = ctx.Err()
	}

	if shouldInvalidateTrust(err) {
		r.Trust.invalidate()
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return result, err
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-keyring").
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(r.Config.Namespace, r.Config.IssuerSecretName, r.Config.KeyringSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(r.Config))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

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
		// Private write-ahead material must be durable before publishing its root.
		if issuerChanged {
			if err := ctx.Err(); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Update(ctx, credentials.issuer); err != nil {
				return ctrl.Result{}, err
			}
		}

		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, credentials.shared); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.pruneIssuerMaterial(ctx, &credentials); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: max(time.Second, credentials.rotation.nextTransition().Sub(now))}, nil
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
	candidate := c.bundle
	if candidate.Generation < math.MaxUint64 {
		candidate.Generation++
	}

	encoded, err := wire.EncodeBundle(candidate)
	if err != nil {
		return false, err
	}

	previous, err := wire.DecodeBundle(bytes.NewReader(c.shared.Data["bundle.json"]))
	if err != nil {
		return false, err
	}

	previous.Generation = candidate.Generation

	previousEncoded, err := wire.EncodeBundle(previous)
	if err != nil {
		return false, err
	}

	stateBytes, err := json.Marshal(c.rotation)
	if err != nil {
		return false, err
	}

	if bytes.Equal(encoded, previousEncoded) && bytes.Equal(stateBytes, c.shared.Data["rotation.json"]) {
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

	s.NextRotation = now.Add(r.Config.Rotation.Interval - r.Config.Rotation.PrepareFor)

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

	return ctrl.Result{RequeueAfter: max(time.Second, r.Config.Rotation.Interval-r.Config.Rotation.PrepareFor)}, nil
}

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState is controller-only metadata beside bundle.json in the shared
// Secret. Primary timestamps suffice to derive scheduling deadlines after restart.
type RotationState struct {
	NextRotation   time.Time            `json:"next_rotation"`
	ActivateAt     time.Time            `json:"activate_at"`
	ActiveIssuer   string               `json:"active_issuer"`
	PreparedIssuer string               `json:"prepared_issuer"`
	Retiring       map[string]time.Time `json:"retiring"`
}

func rootID(der []byte) string { sum := sha256.Sum256(der); return hex.EncodeToString(sum[:]) }
func keyID(k wire.CacheKey) string {
	return string(k.Key.Cache) + "/" + string(k.Key.Purpose) + "/" + hex.EncodeToString(k.Key.ID)
}

func keyScope(k wire.CacheKey) string { return string(k.Key.Cache) + "/" + string(k.Key.Purpose) }

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

// PlanRotation owns its output and plans transitions using the supplied policy.
// Deadlines are measured from actual transitions, never advanced through
// missed intervals after downtime. Issuer staging is done by reconcileKeys before
// this planner; publication persists private material first.
func PlanRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time) (wire.KeyringBundle, RotationState, error) {
	var creationGeneration wire.Generation
	if b.Generation < math.MaxUint64 {
		creationGeneration = b.Generation + 1
	}

	return planRotation(policy, b, s, catalog, now, creationGeneration)
}

// creationGeneration is the publication that will first contain new keys. Zero
// forbids key creation when generations are exhausted, while allowing idle plans.
func planRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time, creationGeneration wire.Generation) (wire.KeyringBundle, RotationState, error) {
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
				s.Retiring[keyID(*k)] = now.Add(policy.RetainFor)
			}
		}

		for i := range b.CacheKeys {
			if b.CacheKeys[i].State == wire.PreparedKey {
				b.CacheKeys[i].State = wire.ActiveKey
			}
		}

		s.Retiring[s.ActiveIssuer] = now.Add(policy.RetainFor)
		s.ActiveIssuer, s.PreparedIssuer = s.PreparedIssuer, ""
		s.ActivateAt = time.Time{}
		s.NextRotation = now.Add(policy.Interval - policy.PrepareFor)
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
		s.ActivateAt = now.Add(policy.PrepareFor)
	}

	candidate := b
	if creationGeneration > candidate.Generation {
		candidate.Generation = creationGeneration
	}

	if _, err := wire.EncodeBundle(candidate); err != nil {
		return b, s, err
	}

	return b, s, nil
}

func containsRoot(b wire.KeyringBundle, id string) bool {
	for _, root := range b.PeerTrustRoots {
		if rootID(root) == id {
			return true
		}
	}

	return false
}

// Reserve a conservative DER ceiling for generated Ed25519 roots, including
// serial-number and ASN.1 time length variation. generateIssuer enforces it.
const reservedRootBytes = 1024

// catalogCapacity reserves active + prepared + ceil(retention / cycle) retiring
// generations. Actual activations are at least Interval apart. The
// extra prepared slot is reserved even when the oldest retiree expires before
// preparation. This deliberately favors a stable limit over phase-dependent fit.
func catalogCapacity(cfg Config, b wire.KeyringBundle) (int, error) {
	cycle := cfg.Rotation.Interval

	retiring := cfg.Rotation.RetainFor / cycle
	if cfg.Rotation.RetainFor%cycle != 0 {
		retiring++
	}

	generations := retiring + 2

	rootBytes := reservedRootBytes
	for _, root := range b.PeerTrustRoots {
		rootBytes = max(rootBytes, len(root))
	}

	rootCost := base64.StdEncoding.EncodedLen(rootBytes) + 3 // quotes and comma
	if int64(generations) > int64(wire.MaxBundleBytes/rootCost) {
		return 0, fmt.Errorf("rotation trust reserve: %w", wire.TooLarge)
	}

	// Measure the wire envelope and fixed-width key pair through the real codec.
	// Reserve all 20 generation digits and the longest key state spelling.
	probe := wire.KeyringBundle{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster, Generation: math.MaxUint64, PeerTrustRoots: b.PeerTrustRoots[:1]}

	empty, err := wire.EncodeBundle(probe)
	if err != nil {
		return 0, err
	}

	for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
		id := make([]byte, 16)
		copy(id, "RKG1")
		binary.BigEndian.PutUint64(id[4:12], 1)

		key, err := wire.NewCacheKey(wire.CacheKeyRef{Cache: wire.CacheID(cfg.Cluster), Purpose: purpose, ID: id}, wire.ActiveKey, [32]byte{})
		if err != nil {
			return 0, err
		}

		probe.CacheKeys = append(probe.CacheKeys, key)
	}

	withKeys, err := wire.EncodeBundle(probe)
	if err != nil {
		return 0, err
	}

	pairCost := len(withKeys) - len(empty) + 1 + 2*(len(wire.RetiringKey)-len(wire.ActiveKey))
	envelope := len(empty) - base64.StdEncoding.EncodedLen(len(probe.PeerTrustRoots[0])) - 2

	available := wire.MaxBundleBytes - envelope - int(generations)*rootCost
	if available < 0 {
		return 0, fmt.Errorf("rotation trust reserve: %w", wire.TooLarge)
	}

	return available / (int(generations) * pairCost), nil
}

// keyedCaches is the durable admission record: both active purposes must exist.
// No process-local admission history is needed across leader changes.
func keyedCaches(b wire.KeyringBundle) map[wire.CacheID]bool {
	purposes := map[wire.CacheID]int{}

	for _, key := range b.CacheKeys {
		if key.State == wire.ActiveKey {
			purposes[key.Key.Cache]++
		}
	}

	ids := make(map[wire.CacheID]bool, len(purposes))
	for id, count := range purposes {
		ids[id] = count == 2
	}

	return ids
}

// admitCatalog retains existing UIDs before filling free slots in BuildCatalog's
// UID order. New low UIDs cannot evict working caches. Deletion frees a slot;
// recreation is a new identity. Rejections are input diagnostics, not key errors.
func admitCatalog(ctx context.Context, cfg Config, catalog []wire.CacheDefinition, b wire.KeyringBundle) ([]wire.CacheDefinition, error) {
	capacity, err := catalogCapacity(cfg, b)
	if err != nil {
		return nil, err
	}

	admitted := keyedCaches(b)
	existing := 0

	for _, cache := range catalog {
		if admitted[cache.ID] {
			existing++
		}
	}

	if existing > capacity {
		// An older controller or changed policy can have overcommitted durable
		// state. Never silently evict its keys or shorten retirement to make room.
		return nil, fmt.Errorf("admitted catalog exceeds rotation capacity %d: %w", capacity, wire.TooLarge)
	}

	slots := capacity - existing

	accepted := make([]wire.CacheDefinition, 0, min(len(catalog), capacity))
	for _, cache := range catalog {
		if !admitted[cache.ID] {
			if slots == 0 {
				ctrl.LoggerFrom(ctx).Info("cache catalog admission rejected", "cache", cache.Name, "uid", cache.ID, "reason", "rotation_capacity", "capacity", capacity)
				continue
			}

			slots--
		}

		accepted = append(accepted, cache)
	}

	return accepted, nil
}
