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
	settings frozenConfig
	client.Client
	APIReader   client.Reader
	Config      Config
	Trust       *Trust
	Now         func() time.Time
	CatalogGate *CatalogGate
}

func (r *KeyringReconciler) runtimeConfig() Config { return r.settings.get(&r.Config) }

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

		state, err = loadSigning(ctx, r.APIReader, r.runtimeConfig(), r.now())
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

	// A dependency's own deadline is retryable while leadership is still live.
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return result, err
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.runtimeConfig()

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-credentials").
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
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
	cfg := r.runtimeConfig()

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := cfg.Validate(); err != nil {
		return ctrl.Result{}, err
	}

	version, _, err := readVersion(ctx, r.APIReader, cfg)
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

	if !validCredentialClaim(cfg, claim) {
		return ctrl.Result{}, wire.Unavailable
	}

	credentials, err := readBoundCredentials(ctx, r.APIReader, cfg, claim, version)
	if err != nil {
		return ctrl.Result{}, err
	}

	catalog, err = admitCatalog(ctx, cfg, catalog, credentials.bundle)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	credentials.discardStalePreparation(cfg, now)

	if err := credentials.prepareIssuer(cfg, now); err != nil {
		return ctrl.Result{}, err
	}

	var encoded []byte

	credentials.bundle, credentials.rotation, encoded, err = planRotation(cfg.Rotation, credentials.bundle, credentials.rotation, catalog, now, nextGeneration(credentials.bundle.Generation))
	if err != nil {
		return ctrl.Result{}, err
	}

	bundleChanged, err := credentials.encodeRotation(encoded)
	if err != nil {
		return ctrl.Result{}, err
	}

	if bundleChanged {
		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, credentials.secret); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: max(time.Second, credentials.rotation.nextTransition().Sub(now))}, nil
}

func (c *credentialState) discardStalePreparation(cfg Config, now time.Time) {
	b, s := &c.bundle, &c.rotation
	// Downtime may exhaust a staged root's useful lifetime. Cancel that unused
	// preparation and stage a fresh replacement with a full new preparation delay.
	if s.PreparedIssuer != "" {
		cert := c.signing[s.PreparedIssuer].certificate

		activation := now
		if s.ActivateAt.After(activation) {
			activation = s.ActivateAt
		}

		// This issuer must sign until its replacement activates one full interval
		// after actual activation, and cover the last leaf's entire lifetime.
		if activation.Add(cfg.Rotation.Interval + cfg.CertificateLifetime).After(cert.NotAfter) {
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

func (c *credentialState) prepareIssuer(cfg Config, now time.Time) error {
	b, s, material := &c.bundle, &c.rotation, &c.material

	if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		cert, key, err := generateIssuer(now, cfg)
		if err != nil {
			return err
		}

		id := rootID(cert)
		material.Keys[id] = signingMaterial{Certificate: cert, PrivateKey: key}
		b.PeerTrustRoots = append(b.PeerTrustRoots, cert)
		s.PreparedIssuer = id
	}

	return nil
}

// encodeRotation validates the complete candidate, including its publication
// generation, before the single Secret CAS. Reuse the planner's encoded bundle.
func (c *credentialState) encodeRotation(encoded []byte) (bool, error) {
	clean := issuerMaterial{Keys: map[string]signingMaterial{}}

	for _, root := range c.bundle.PeerTrustRoots {
		id := rootID(root)
		clean.Keys[id] = c.material.Keys[id]
	}

	c.material = clean
	if err := c.validateRotation(); err != nil {
		return false, err
	}

	if c.bundle.Generation == c.generation {
		return false, nil
	}

	stateBytes, err := json.Marshal(c.rotation)
	if err != nil {
		return false, err
	}

	materialBytes, err := json.Marshal(c.material)
	if err != nil {
		return false, err
	}

	c.secret.Data["bundle.json"] = encoded
	c.secret.Data["rotation.json"] = stateBytes
	c.secret.Data["issuer.json"] = materialBytes

	return true, nil
}

func (r *KeyringReconciler) initializeKeys(ctx context.Context, version *corev1.ConfigMap, catalog []wire.CacheDefinition) (ctrl.Result, error) {
	cfg := r.runtimeConfig()

	existing := &corev1.Secret{}

	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.CredentialsSecretName}, existing)
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return ctrl.Result{}, err
		}

		if version.Annotations[initializationProtocol] == stagedInitialization {
			return r.commitStagedCredentials(ctx, version, existing)
		}

		return ctrl.Result{}, wire.Unavailable
	}

	now := r.now()

	cert, key, err := generateIssuer(now, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	id := rootID(cert)
	b := wire.KeyringBundle{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster, Generation: 1, PeerTrustRoots: [][]byte{cert}}
	s := RotationState{ActiveIssuer: id, NextRotation: now.Add(cfg.Rotation.Interval), Retiring: map[string]time.Time{}}

	catalog, err = admitCatalog(ctx, cfg, catalog, b)
	if err != nil {
		return ctrl.Result{}, err
	}

	var encoded []byte

	b, s, encoded, err = planRotation(cfg.Rotation, b, s, catalog, now, 1)
	if err != nil {
		return ctrl.Result{}, err
	}

	s.NextRotation = now.Add(cfg.Rotation.Interval - cfg.Rotation.PrepareFor)

	// The permanent claim is on the already-required version object. Topology
	// preserves annotations with its CAS. Missing Secrets after this claim never
	// authorize Create on recovery, even when the first create response was lost.
	claim := fmt.Sprintf("%s/%s", cfg.CredentialsSecretName, id)

	secret := credentialSecret(cfg, cfg.CredentialsSecretName, claim)
	material := issuerMaterial{Keys: map[string]signingMaterial{id: {Certificate: cert, PrivateKey: key}}}

	secret.Data["issuer.json"], err = json.Marshal(material)
	if err != nil {
		return ctrl.Result{}, err
	}

	secret.Data["bundle.json"] = encoded

	secret.Data["rotation.json"], err = json.Marshal(s)
	if err != nil {
		return ctrl.Result{}, err
	}

	candidate := credentialState{bundle: b, rotation: s, material: material}
	if err := candidate.validateRotation(); err != nil {
		return ctrl.Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if version.Annotations[initializationProtocol] == stagedInitialization {
		// Unlike the legacy path, the claim is committed only after the complete
		// Secret exists. Until then readers cannot use this candidate.
		secret.Annotations[installationUIDAnnotation] = version.Annotations[installationUIDAnnotation]

		secret.Annotations[initializationProtocol] = stagedInitialization
		if err := r.Create(ctx, secret); err != nil {
			return ctrl.Result{}, err
		}

		return r.commitStagedCredentials(ctx, version, secret)
	}

	version.Annotations[credentialClaim] = claim
	if err := r.Update(ctx, version); err != nil {
		return ctrl.Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, secret); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: max(time.Second, cfg.Rotation.Interval-cfg.Rotation.PrepareFor)}, nil
}

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState is controller-only metadata beside bundle.json in the credentials
// Secret. Primary timestamps suffice to derive scheduling deadlines after restart.
type RotationState struct {
	NextRotation   time.Time            `json:"next_rotation"`
	ActivateAt     time.Time            `json:"activate_at"`
	ActiveIssuer   string               `json:"active_issuer"`
	PreparedIssuer string               `json:"prepared_issuer"`
	Retiring       map[string]time.Time `json:"retiring"` // Root fingerprints only.
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
// this planner; publication persists all credentials atomically.
func PlanRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time) (wire.KeyringBundle, RotationState, error) {
	b, s, _, err := planRotation(policy, b, s, catalog, now, nextGeneration(b.Generation))
	return b, s, err
}

func nextGeneration(g wire.Generation) wire.Generation {
	if g == math.MaxUint64 {
		return 0
	}

	return g + 1
}

// creationGeneration is the publication that will first contain new keys. Zero
// forbids key creation when generations are exhausted, while allowing idle plans.
func planRotation(policy RotationPolicy, b wire.KeyringBundle, s RotationState, catalog []wire.CacheDefinition, now time.Time, creationGeneration wire.Generation) (wire.KeyringBundle, RotationState, []byte, error) {
	// Keep wire validation and the encoded size bound at the input boundary.
	// Ownership does not require decoding the just-validated representation.
	if _, err := wire.EncodeBundle(b); err != nil {
		return wire.KeyringBundle{}, RotationState{}, nil, err
	}

	original, originalState := b, s

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
			return b, s, nil, wire.InvalidRequest
		}

		wanted[cache.ID] = true
	}

	keys := b.CacheKeys[:0]
	for _, k := range b.CacheKeys {
		if !wanted[k.Key.Cache] {
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
					return b, s, nil, err
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

		keys := b.CacheKeys[:0]
		for _, k := range b.CacheKeys {
			// A cache added during preparation can have only its initial active key.
			if k.State == wire.ActiveKey && prepared[keyScope(k)] {
				continue
			}

			if k.State == wire.PreparedKey {
				k.State = wire.ActiveKey
			}

			keys = append(keys, k)
		}

		b.CacheKeys = keys

		s.Retiring[s.ActiveIssuer] = now.Add(policy.RetainFor)
		s.ActiveIssuer, s.PreparedIssuer = s.PreparedIssuer, ""
		s.ActivateAt = time.Time{}
		s.NextRotation = now.Add(policy.Interval - policy.PrepareFor)
	} else if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		if s.PreparedIssuer == "" {
			return b, s, nil, wire.Unavailable
		}

		var prepared []wire.CacheKey

		for _, k := range b.CacheKeys {
			if k.State != wire.ActiveKey {
				continue
			}

			next, err := newCacheKey(k.Key.Cache, k.Key.Purpose, wire.PreparedKey, creationGeneration)
			if err != nil {
				return b, s, nil, err
			}

			prepared = append(prepared, next)
		}

		b.CacheKeys = append(b.CacheKeys, prepared...)
		s.ActivateAt = now.Add(policy.PrepareFor)
	}

	// Generation is the final publication version, not a rotation ordinal.
	// Normalize empty collections for the comparison without consuming a version.
	if len(original.CacheKeys) == 0 {
		original.CacheKeys = []wire.CacheKey{}
	}

	if originalState.Retiring == nil {
		originalState.Retiring = map[string]time.Time{}
	}

	if !reflect.DeepEqual(original, b) || !reflect.DeepEqual(originalState, s) {
		if creationGeneration == 0 {
			return b, s, nil, wire.Unavailable
		}

		b.Generation = creationGeneration
	}

	encoded, err := wire.EncodeBundle(b)

	return b, s, encoded, err
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

// catalogCapacity reserves active + prepared key generations and
// active + prepared + ceil(retention / cycle) roots. Actual activations are at
// least Interval apart. The extra prepared slot is reserved even when the oldest
// retiree expires before preparation. This favors a stable limit over phase-dependent fit.
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

	for i, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
		id := make([]byte, 16)
		copy(id, "RKG1")
		binary.BigEndian.PutUint64(id[4:12], 1)

		key, err := wire.NewCacheKey(wire.CacheKeyRef{Cache: wire.CacheID(cfg.Cluster), Purpose: purpose, ID: id}, wire.ActiveKey, [32]byte{byte(i)})
		if err != nil {
			return 0, err
		}

		probe.CacheKeys = append(probe.CacheKeys, key)
	}

	withKeys, err := wire.EncodeBundle(probe)
	if err != nil {
		return 0, err
	}

	pairCost := len(withKeys) - len(empty) + 1 + 2*(len(wire.PreparedKey)-len(wire.ActiveKey))
	envelope := len(empty) - base64.StdEncoding.EncodedLen(len(probe.PeerTrustRoots[0])) - 2

	available := wire.MaxBundleBytes - envelope - int(generations)*rootCost
	if available < 0 {
		return 0, fmt.Errorf("rotation trust reserve: %w", wire.TooLarge)
	}

	return available / (2 * pairCost), nil
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
