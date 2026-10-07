// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type credentials struct {
	client.Writer
	APIReader client.Reader
	Config    Config
	Trust     *trustStore
	Now       func() time.Time
}

// ReconcileCredentials completes authoritative post-write signing validation
// before releasing admission. Scheduling and conflict retries belong to root.
func (a *Authority) ReconcileCredentials(ctx context.Context) (time.Duration, error) {
	r := a.credentials
	if err := a.gate.Acquire(ctx); err != nil {
		return 0, err
	}
	defer a.gate.Release()

	result, err := r.reconcileKeys(ctx)

	// Refresh committed trust while the catalog gate is still held. Failed gate
	// admission must not reach this completion path or withdraw accepted trust.
	// Install only after authoritative validation of the committed credentials,
	// including installation binding, rotation consistency, and signing lifetime.
	if err == nil {
		var state signingState

		state, err = loadSigning(ctx, r.APIReader, r.Config, credentialTime(r.Now))
		if err == nil {
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

	return result.RequeueAfter, err
}

// Credential timestamps use the same precision as X.509 validity times.
func credentialTime(now func() time.Time) time.Time {
	if now != nil {
		return now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

func credentialSecret(cfg Config, name, claim string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: name, Annotations: map[string]string{credentialClaim: claim}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
}

func (r *credentials) reconcileKeys(ctx context.Context) (ctrl.Result, error) {
	cfg := r.Config

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

	catalog, err := members.BuildCatalog(caches.Items)
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

	if err := r.Trust.validateReplay(credentials.bundle); err != nil {
		return ctrl.Result{}, err
	}

	return r.rotateKeys(ctx, cfg, credentials, catalog)
}

func (r *credentials) rotateKeys(ctx context.Context, cfg Config, credentials credentialState, catalog []wire.CacheDefinition) (ctrl.Result, error) {
	var err error

	catalog, err = admitCatalog(ctx, cfg, catalog, credentials.bundle)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := credentialTime(r.Now)
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

func (r *credentials) initializeKeys(ctx context.Context, version *corev1.ConfigMap, catalog []wire.CacheDefinition) (ctrl.Result, error) {
	cfg := r.Config

	existing := &corev1.Secret{}

	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.CredentialsSecretName}, existing)
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return ctrl.Result{}, err
		}

		return r.commitStagedCredentials(ctx, version, existing)
	}

	now := credentialTime(r.Now)

	cert, key, err := generateIssuer(now, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	id := rootID(cert)
	b := wire.KeyringBundle{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster, Generation: 1, PeerTrustRoots: [][]byte{cert}}
	s := rotationState{ActiveIssuer: id, NextRotation: now.Add(cfg.Rotation.Interval), Retiring: map[string]time.Time{}}

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

	return r.createInitialCredentials(ctx, version, secret)
}

func (r *credentials) createInitialCredentials(ctx context.Context, version *corev1.ConfigMap, secret *corev1.Secret) (ctrl.Result, error) {
	// Commit the claim only after the complete Secret exists. Until then readers
	// cannot use this candidate. Recovery must bind this exact Kubernetes UID.
	secret.Annotations[installationUIDAnnotation] = version.Annotations[installationUIDAnnotation]

	secret.Annotations[initializationProtocol] = stagedInitialization
	if err := r.Create(ctx, secret); err != nil {
		return ctrl.Result{}, err
	}

	return r.commitStagedCredentials(ctx, version, secret)
}

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState is controller-only metadata beside bundle.json in the credentials
// Secret. Primary timestamps suffice to derive scheduling deadlines after restart.
type rotationState struct {
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

func (s rotationState) nextTransition() time.Time {
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

func nextGeneration(g wire.Generation) wire.Generation {
	if g == math.MaxUint64 {
		return 0
	}

	return g + 1
}

// planRotation owns its output. Deadlines start at actual transitions, not missed
// intervals. reconcileKeys stages issuers before planning and commits atomically.
// creationGeneration is the publication that will first contain new keys. Zero
// forbids key creation when generations are exhausted, while allowing idle plans.
func planRotation(policy RotationPolicy, b wire.KeyringBundle, s rotationState, catalog []wire.CacheDefinition, now time.Time, creationGeneration wire.Generation) (wire.KeyringBundle, rotationState, []byte, error) {
	// Keep wire validation and the encoded size bound at the input boundary.
	// Ownership does not require decoding the just-validated representation.
	if _, err := wire.EncodeBundle(b); err != nil {
		return wire.KeyringBundle{}, rotationState{}, nil, err
	}

	original, originalState := b, s
	b, s = cloneRotation(b, s)

	wanted, err := rotationCatalog(catalog)
	if err != nil {
		return b, s, nil, err
	}

	pruneRotation(&b, &s, wanted, now)

	if err := addMissingCacheKeys(&b, catalog, creationGeneration); err != nil {
		return b, s, nil, err
	}

	if !s.ActivateAt.IsZero() && !now.Before(s.ActivateAt) {
		activateRotation(&b, &s, policy, now)
	} else if s.ActivateAt.IsZero() && !now.Before(s.NextRotation) {
		if err := prepareCacheKeys(&b, &s, policy, now, creationGeneration); err != nil {
			return b, s, nil, err
		}
	}
	// Generation counts changed publications, not rotation cycles.
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

func rotationCatalog(catalog []wire.CacheDefinition) (map[wire.CacheID]bool, error) {
	wanted := make(map[wire.CacheID]bool, len(catalog))
	for _, cache := range catalog {
		if !wire.ValidUUID(string(cache.ID)) || wanted[cache.ID] {
			return nil, wire.InvalidRequest
		}

		wanted[cache.ID] = true
	}

	return wanted, nil
}

func cloneRotation(b wire.KeyringBundle, s rotationState) (wire.KeyringBundle, rotationState) {
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

	return b, s
}

func pruneRotation(b *wire.KeyringBundle, s *rotationState, wanted map[wire.CacheID]bool, now time.Time) {
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
}

func addMissingCacheKeys(b *wire.KeyringBundle, catalog []wire.CacheDefinition, generation wire.Generation) error {
	present := map[string]bool{}
	for _, key := range b.CacheKeys {
		present[keyScope(key)] = true
	}

	for _, cache := range catalog {
		for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
			if !present[string(cache.ID)+"/"+string(purpose)] {
				k, err := newCacheKey(cache.ID, purpose, wire.ActiveKey, generation)
				if err != nil {
					return err
				}

				b.CacheKeys = append(b.CacheKeys, k)
			}
		}
	}

	return nil
}

func activateRotation(b *wire.KeyringBundle, s *rotationState, policy RotationPolicy, now time.Time) {
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
}

func prepareCacheKeys(b *wire.KeyringBundle, s *rotationState, policy RotationPolicy, now time.Time, generation wire.Generation) error {
	if s.PreparedIssuer == "" {
		return wire.Unavailable
	}

	var prepared []wire.CacheKey

	for _, k := range b.CacheKeys {
		if k.State != wire.ActiveKey {
			continue
		}

		next, err := newCacheKey(k.Key.Cache, k.Key.Purpose, wire.PreparedKey, generation)
		if err != nil {
			return err
		}

		prepared = append(prepared, next)
	}

	b.CacheKeys = append(b.CacheKeys, prepared...)
	s.ActivateAt = now.Add(policy.PrepareFor)

	return nil
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

// Trust atomically holds controller-validated public roots and the matching
// delivery bundle. Requests never refresh this state or fall back to Kubernetes.
// A failed observation cannot restore withdrawn trust.
type trustStore struct {
	mu        sync.RWMutex
	roots     *x509.CertPool
	bundle    *acceptedKeyring
	changed   chan struct{}
	confirmed time.Time
	maxAge    time.Duration
	// Retain only non-secret replay protection when serving state is withdrawn.
	// Otherwise a rejected rollback could be accepted on the next reconcile.
	highWater wire.Generation
	digest    [sha256.Size]byte
	authority context.Context
	revoke    context.CancelFunc
	process   context.Context
}

// acceptedKeyring owns an immutable, bounded wire encoding, never issuer material.
// Polls share it without copying secret bytes per waiting request.
type acceptedKeyring struct {
	generation wire.Generation
	encoded    string
}

func (*acceptedKeyring) String() string   { return "<redacted keyring>" }
func (*acceptedKeyring) GoString() string { return "<redacted keyring>" }

func (t *trustStore) validateReplay(bundle wire.KeyringBundle) error {
	encoded, err := wire.EncodeBundle(bundle)
	if err != nil {
		return err
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.validateReplayLocked(bundle.Generation, sha256.Sum256(encoded))
}

func (t *trustStore) validateReplayLocked(generation wire.Generation, digest [sha256.Size]byte) error {
	if generation < t.highWater || generation == t.highWater && digest != t.digest {
		return wire.Conflict
	}

	return nil
}

func (t *trustStore) install(ctx context.Context, roots *x509.CertPool, bundle wire.KeyringBundle) error {
	encoded, err := wire.EncodeBundle(bundle)
	if err != nil {
		return err
	}

	accepted := &acceptedKeyring{generation: bundle.Generation, encoded: string(encoded)}
	digest := sha256.Sum256(encoded)

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	if roots == nil {
		return wire.Unavailable
	}

	if err := t.validateReplayLocked(accepted.generation, digest); err != nil {
		return err
	}

	if t.bundle != nil && accepted.generation == t.highWater {
		accepted = t.bundle
	}

	t.highWater, t.digest = accepted.generation, digest
	t.roots = roots

	t.confirmed = time.Now()
	if t.authority == nil || t.authority.Err() != nil {
		t.authority, t.revoke = context.WithCancel(context.Background())
	}

	t.bundle = accepted
	t.notifyLocked()

	return nil
}

func (t *trustStore) notifyLocked() {
	if t.changed != nil {
		close(t.changed)
	}

	t.changed = make(chan struct{})
}

// Caller holds mu so admission and accepted bundle are captured atomically.
// Invalidation revokes admission across recovery. Rotation and reconfirmation
// preserve it without extending its captured freshness deadline.
func (t *trustStore) admitLocked(parent context.Context) (*Admission, context.CancelFunc, error) {
	if t.process != nil && t.process.Err() != nil {
		return nil, nil, wire.Unavailable
	}

	if t.roots == nil || t.authority == nil || t.authority.Err() != nil || t.maxAge > 0 && time.Since(t.confirmed) >= t.maxAge {
		return nil, nil, wire.Unavailable
	}

	expiry := time.Unix(1<<62, 0)
	if t.maxAge > 0 {
		expiry = t.confirmed.Add(t.maxAge)
	}

	guard, cancel := newAdmission(parent, t.authority, expiry)
	guard.trust, guard.bundle = true, t.bundle

	guard.process = t.process
	if t.process != nil {
		stop := context.AfterFunc(t.process, cancel)
		return guard, func() { stop(); cancel() }, nil
	}

	return guard, cancel, nil
}

func (t *trustStore) invalidate() {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()

		t.roots, t.bundle = nil, nil
		if t.revoke != nil {
			t.revoke()
		}

		t.notifyLocked()
	}
}

func (t *trustStore) keyring() (*acceptedKeyring, <-chan struct{}, error) {
	if t == nil {
		return nil, nil, wire.Unavailable
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.roots == nil || t.bundle == nil || t.maxAge > 0 && time.Since(t.confirmed) >= t.maxAge {
		return nil, nil, wire.Unavailable
	}

	return t.bundle, t.changed, nil
}

func (t *trustStore) waitKeyring(ctx context.Context, after *wire.Generation) (*acceptedKeyring, error) {
	timer := time.NewTimer(wire.PollWait)
	defer timer.Stop()

	expired := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		current, changed, err := t.keyring()
		if err != nil {
			return nil, err
		}

		if after == nil || *after < current.generation && *after != 0 {
			return current, nil
		}

		if *after == 0 || *after > current.generation {
			return nil, wire.Conflict
		}

		if expired {
			return nil, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-timer.C:
			expired = true
		}
	}
}

// pool is immutable after installation, including when shared with TLS configs.
func (t *trustStore) pool() (*x509.CertPool, error) {
	if t == nil {
		return nil, wire.Unavailable
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.roots == nil || t.maxAge > 0 && time.Since(t.confirmed) >= t.maxAge {
		return nil, wire.Unavailable
	}

	return t.roots, nil
}

// An unsuccessful read supplies no new authority facts. NotFound is an observed
// deletion, unlike an unavailable API. Validation errors are never wrapped here.
type authorityReadError struct{ error }

func (e authorityReadError) Unwrap() error { return e.error }

func authorityReadFailure(err error) error {
	if apierrors.IsNotFound(err) {
		return err
	}

	return authorityReadError{err}
}

// shouldInvalidateTrust is a fail-closed policy, not proof of invalid authority.
// Only an authorityReadError preserves accepted trust; every other non-nil error
// invalidates it, including NotFound, validation, write, and unclassified failures.
// Unwrapped cancellation also invalidates; callers that fail gate admission return
// before applying this policy because they have not started observing authority.
func shouldInvalidateTrust(err error) bool {
	var unread authorityReadError
	return err != nil && !errors.As(err, &unread)
}

const credentialClaim = "racer.unbounded-cloud.io/credentials"

func validCredentialClaim(cfg Config, claim string) bool {
	name, fingerprint, ok := strings.Cut(claim, "/")
	decoded, err := hex.DecodeString(fingerprint)

	return ok && name == cfg.CredentialsSecretName && err == nil && len(decoded) == sha256.Size && fingerprint == hex.EncodeToString(decoded)
}

type credentialState struct {
	secret     *corev1.Secret
	bundle     wire.KeyringBundle
	rotation   rotationState
	material   issuerMaterial
	generation wire.Generation
	// Parsed once per authoritative read, never used to install candidate trust.
	signing map[string]parsedSigning
}

func readBoundCredentials(ctx context.Context, reader client.Reader, cfg Config, claim string, version *corev1.ConfigMap) (credentialState, error) {
	var secret corev1.Secret

	if err := ctx.Err(); err != nil {
		return credentialState{}, err
	}

	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.CredentialsSecretName}, &secret); err != nil {
		return credentialState{}, authorityReadFailure(err)
	}

	if !validCredentialClaim(cfg, claim) || secret.Annotations[credentialClaim] != claim || secret.DeletionTimestamp != nil || secret.ResourceVersion == "" {
		return credentialState{}, wire.Unavailable
	}

	if version.Annotations[credentialClaim] != claim || secret.UID == "" || version.Annotations[credentialUID] != string(secret.UID) || secret.Annotations[initializationProtocol] != stagedInitialization || secret.Annotations[installationUIDAnnotation] != version.Annotations[installationUIDAnnotation] {
		return credentialState{}, wire.Unavailable
	}

	return decodeCredentials(cfg, &secret)
}

func decodeCredentials(cfg Config, secret *corev1.Secret) (credentialState, error) {
	b, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	if err != nil {
		return credentialState{}, wire.Unavailable
	}

	var (
		s        rotationState
		material issuerMaterial
	)

	if b.Cluster != cfg.Cluster || decodeCredentialMetadata(secret.Data["rotation.json"], &s) != nil || decodeCredentialMetadata(secret.Data["issuer.json"], &material) != nil {
		return credentialState{}, wire.Unavailable
	}

	credentials := credentialState{secret: secret, bundle: b, rotation: s, material: material, generation: b.Generation}
	if err := credentials.validateRotation(); err != nil {
		return credentialState{}, err
	}

	return credentials, nil
}

func decodeCredentialMetadata(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(out); err != nil {
		return err
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return wire.Unavailable
	}

	return nil
}

func (c *credentialState) validateRotation() error {
	b, s, m := c.bundle, c.rotation, c.material
	if len(m.Keys) != len(b.PeerTrustRoots) || s.NextRotation.IsZero() || s.Retiring == nil || !containsRoot(b, s.ActiveIssuer) || (s.PreparedIssuer == "") != s.ActivateAt.IsZero() {
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

	if !s.ActivateAt.IsZero() {
		if !containsRoot(b, s.PreparedIssuer) || s.PreparedIssuer == s.ActiveIssuer || !s.ActivateAt.After(s.NextRotation) {
			return wire.Unavailable
		}
	}

	if err := c.validateRetiringRoots(); err != nil {
		return err
	}

	return validatePreparedKeys(b.CacheKeys, s.ActivateAt)
}

func (c *credentialState) validateRetiringRoots() error {
	b, s := c.bundle, c.rotation
	required := map[string]struct{}{}

	for _, root := range b.PeerTrustRoots {
		id := rootID(root)

		key, ok := c.signing[id]
		if !ok || !bytes.Equal(key.certificate.Raw, root) {
			return wire.Unavailable
		}

		if id != s.ActiveIssuer && id != s.PreparedIssuer {
			required[id] = struct{}{}
		}
	}

	if len(required) != len(s.Retiring) {
		return wire.Unavailable
	}

	for id := range required {
		if at, ok := s.Retiring[id]; !ok || at.IsZero() {
			return wire.Unavailable
		}
	}

	return nil
}

func validatePreparedKeys(keys []wire.CacheKey, activateAt time.Time) error {
	prepared := map[string]bool{}

	for _, key := range keys {
		if key.State != wire.PreparedKey {
			continue
		}

		scope := keyScope(key)
		if activateAt.IsZero() || prepared[scope] {
			return wire.Unavailable
		}

		prepared[scope] = true
	}

	return nil
}

// CatalogGate serializes authoritative catalog and credential operations while
// allowing callers to abandon admission when their context is canceled.
type catalogGate struct {
	token chan struct{}
}

func newCatalogGate() *catalogGate {
	g := &catalogGate{token: make(chan struct{}, 1)}
	g.token <- struct{}{}

	return g
}

// Acquire returns ownership only for a live context. A failed acquisition must
// not be released and does not constitute an observation of invalid authority.
func (g *catalogGate) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
		if err := ctx.Err(); err != nil {
			g.Release()
			return err
		}

		return nil
	}
}

// Release ends a successfully acquired critical section.
func (g *catalogGate) Release() {
	g.token <- struct{}{}
}

const installationUIDAnnotation = "racer.unbounded-cloud.io/installation-uid"

func readInstallation(ctx context.Context, reader client.Reader, cfg Config, fresh bool) (*corev1.ConfigMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, cm); err != nil {
		return nil, authorityReadFailure(err)
	}

	return cm, validateMarker(cm, cfg, fresh)
}

func validateMarker(cm *corev1.ConfigMap, cfg Config, fresh bool) error {
	state := "consumed"
	if fresh {
		state = "fresh"
	}

	immutable := cm.Immutable != nil && *cm.Immutable
	if cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Data[markerInitializationProtocol] != stagedInitialization || cm.Data["cluster"] != string(cfg.Cluster) || cm.Data["version_configmap"] != cfg.VersionConfigMapName || cm.Data["state"] != state || immutable == fresh {
		return fmt.Errorf("installation marker invalid: %w", wire.Unavailable)
	}

	return nil
}

// ensureInstalled recovers only staged-v1 installations with UID-bound authority.
func ensureInstalled(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config) error {
	if !wire.ValidUUID(string(cfg.Cluster)) {
		return wire.InvalidRequest
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	marker := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, marker); err != nil {
		return err
	}

	return ensureStagedInstallation(ctx, writer, reader, cfg, marker)
}

func versionData(v versionRecord) map[string]string {
	return map[string]string{"cluster": string(v.Cluster), "sequence": strconv.FormatUint(uint64(v.Sequence), 10), "membership_version": strconv.FormatUint(uint64(v.MembershipVersion), 10), "content_hash": v.ContentHash, "membership_hash": v.MembershipHash}
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func (v versionRecord) valid() bool {
	return wire.ValidUUID(string(v.Cluster)) && v.Sequence > 0 && v.MembershipVersion > 0 && uint64(v.MembershipVersion) <= uint64(v.Sequence) && validHash(v.ContentHash) && validHash(v.MembershipHash)
}

func parseVersion(cm *corev1.ConfigMap, cluster wire.ClusterID, markerUID types.UID) (versionRecord, error) {
	sequence, e1 := strconv.ParseUint(cm.Data["sequence"], 10, 64)
	membership, e2 := strconv.ParseUint(cm.Data["membership_version"], 10, 64)

	v := versionRecord{Cluster: wire.ClusterID(cm.Data["cluster"]), Sequence: wire.Sequence(sequence), MembershipVersion: wire.MembershipVersion(membership), ContentHash: cm.Data["content_hash"], MembershipHash: cm.Data["membership_hash"]}
	if e1 != nil || e2 != nil || !v.valid() || v.Cluster != cluster || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Annotations[installationUIDAnnotation] != string(markerUID) || strconv.FormatUint(sequence, 10) != cm.Data["sequence"] || strconv.FormatUint(membership, 10) != cm.Data["membership_version"] {
		return versionRecord{}, fmt.Errorf("durable version state invalid; explicit new-cluster rebootstrap required: %w", wire.Unavailable)
	}

	return v, nil
}

func readVersion(ctx context.Context, reader client.Reader, cfg Config) (*corev1.ConfigMap, versionRecord, error) {
	marker, err := readInstallation(ctx, reader, cfg, false)
	if err != nil {
		return nil, versionRecord{}, err
	}

	cm := &corev1.ConfigMap{}

	if err := ctx.Err(); err != nil {
		return nil, versionRecord{}, err
	}

	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}, cm); err != nil {
		return nil, versionRecord{}, authorityReadFailure(err)
	}

	v, err := parseVersion(cm, cfg.Cluster, marker.UID)
	if marker.Data[versionUID] == "" || marker.Data[versionUID] != string(cm.UID) || cm.Annotations[initializationProtocol] != stagedInitialization {
		err = wire.Unavailable
	}

	return cm, v, err
}

const (
	initializationProtocol       = "racer.unbounded-cloud.io/initialization"
	markerInitializationProtocol = "initialization_protocol"
	stagedInitialization         = "staged-v1"
	versionUID                   = "version_uid"
	credentialUID                = "racer.unbounded-cloud.io/credentials-uid"
)

// A candidate is not authority: the
// permanent parent CAS binds its Kubernetes UID before any reader can use it.
// Retrying Create cannot restore deleted authority because the UID changes.
func ensureStagedInstallation(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config, marker *corev1.ConfigMap) error {
	for {
		err := stageInstallation(ctx, writer, reader, cfg, marker)
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}

		marker = &corev1.ConfigMap{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, marker); err != nil {
			return err
		}
	}
}

func stageInstallation(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config, marker *corev1.ConfigMap) error {
	if marker.Data[markerInitializationProtocol] != stagedInitialization {
		return wire.Unavailable
	}

	if marker.Data["state"] == "consumed" {
		_, _, err := readVersion(ctx, reader, cfg)
		return err
	}

	if err := validateMarker(marker, cfg, true); err != nil {
		return err
	}

	if marker.Data[versionUID] != "" {
		return wire.Unavailable
	}

	content, membership, err := wire.ContentHashes(wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster})
	if err != nil {
		return err
	}

	data := versionData(versionRecord{Cluster: cfg.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership})
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}

	candidate := &corev1.ConfigMap{}
	if err := reader.Get(ctx, key, candidate); apierrors.IsNotFound(err) {
		candidate = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Annotations: map[string]string{installationUIDAnnotation: string(marker.UID), initializationProtocol: stagedInitialization}}, Data: data}

		if err := ctx.Err(); err != nil {
			return err
		}

		if err := writer.Create(ctx, candidate); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// A concurrent winner may already have committed and advanced the candidate.
	if _, _, err := readVersion(ctx, reader, cfg); err == nil {
		return nil
	}

	if !validStagedVersion(candidate, marker.UID, data) {
		return wire.Unavailable
	}

	marker.Data[versionUID] = string(candidate.UID)
	marker.Data["state"] = "consumed"
	immutable := true
	marker.Immutable = &immutable

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := writer.Update(ctx, marker); err != nil {
		return err
	}

	_, _, err = readVersion(ctx, reader, cfg)

	return err
}

func validStagedVersion(candidate *corev1.ConfigMap, markerUID types.UID, data map[string]string) bool {
	return candidate.UID != "" && candidate.ResourceVersion != "" && candidate.DeletionTimestamp == nil &&
		(candidate.Immutable == nil || !*candidate.Immutable) && candidate.Annotations[installationUIDAnnotation] == string(markerUID) &&
		candidate.Annotations[initializationProtocol] == stagedInitialization && candidate.Annotations[credentialClaim] == "" && reflect.DeepEqual(candidate.Data, data)
}

// Commit only a complete generation-one candidate from this installation. The
// material stays exclusively in the ordinary credentials Secret. No pending
// private-key copy, second Secret, or new RBAC permission is necessary.
func (r *credentials) commitStagedCredentials(ctx context.Context, version *corev1.ConfigMap, secret *corev1.Secret) (ctrl.Result, error) {
	cfg := r.Config

	claim := secret.Annotations[credentialClaim]
	if !validStagedCredentials(cfg, version, secret) {
		return ctrl.Result{}, wire.Unavailable
	}

	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	if err != nil {
		return ctrl.Result{}, wire.Unavailable
	}

	candidate := credentialState{bundle: bundle}
	if bundle.Cluster != cfg.Cluster || bundle.Generation != 1 || decodeCredentialMetadata(secret.Data["rotation.json"], &candidate.rotation) != nil || decodeCredentialMetadata(secret.Data["issuer.json"], &candidate.material) != nil {
		return ctrl.Result{}, wire.Unavailable
	}

	if err := candidate.validateRotation(); err != nil {
		return ctrl.Result{}, err
	}

	if claim != cfg.CredentialsSecretName+"/"+candidate.rotation.ActiveIssuer || candidate.rotation.PreparedIssuer != "" {
		return ctrl.Result{}, wire.Unavailable
	}

	version.Annotations[credentialClaim] = claim
	version.Annotations[credentialUID] = string(secret.UID)

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Update(ctx, version); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: max(time.Second, candidate.rotation.nextTransition().Sub(credentialTime(r.Now)))}, nil
}

func validStagedCredentials(cfg Config, version *corev1.ConfigMap, secret *corev1.Secret) bool {
	return version.Annotations[credentialClaim] == "" && version.Annotations[credentialUID] == "" &&
		secret.UID != "" && secret.ResourceVersion != "" && secret.DeletionTimestamp == nil && (secret.Immutable == nil || !*secret.Immutable) &&
		secret.Annotations[initializationProtocol] == stagedInitialization && secret.Annotations[installationUIDAnnotation] == version.Annotations[installationUIDAnnotation] &&
		validCredentialClaim(cfg, secret.Annotations[credentialClaim])
}
