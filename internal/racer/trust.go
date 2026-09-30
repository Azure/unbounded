// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Trust atomically holds controller-validated public roots and the matching
// delivery bundle. Requests never refresh this state or fall back to Kubernetes.
// A failed observation cannot restore withdrawn trust.
type Trust struct {
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
}

// acceptedKeyring owns an immutable, bounded wire encoding, never issuer material.
// Polls share it without copying secret bytes per waiting request.
type acceptedKeyring struct {
	generation wire.Generation
	encoded    string
}

func (*acceptedKeyring) String() string   { return "<redacted keyring>" }
func (*acceptedKeyring) GoString() string { return "<redacted keyring>" }

func (t *Trust) install(ctx context.Context, roots *x509.CertPool, bundle wire.KeyringBundle) error {
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

	if accepted.generation < t.highWater || accepted.generation == t.highWater && digest != t.digest {
		return wire.Conflict
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

func (t *Trust) notifyLocked() {
	if t.changed != nil {
		close(t.changed)
	}

	t.changed = make(chan struct{})
}

// writeContext captures trust before response authentication/issuance. Explicit
// invalidation permanently revokes that generation, even across recovery. Normal
// validated rotation allows admitted responses to finish, but neither rotation nor
// reconfirmation extends their captured freshness deadline.
func (t *Trust) writeContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if t == nil {
		return nil, nil, wire.Unavailable
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.roots == nil || t.authority == nil || t.authority.Err() != nil || t.maxAge > 0 && time.Since(t.confirmed) >= t.maxAge {
		return nil, nil, wire.Unavailable
	}

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	if t.maxAge > 0 {
		ctx, cancel = context.WithDeadline(parent, t.confirmed.Add(t.maxAge))
	} else {
		ctx, cancel = context.WithCancel(parent)
	}

	stop := context.AfterFunc(t.authority, cancel)

	return ctx, func() { stop(); cancel() }, nil
}

func (t *Trust) invalidate() {
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

func (t *Trust) keyring() (*acceptedKeyring, <-chan struct{}, error) {
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

func (t *Trust) waitKeyring(ctx context.Context, after *wire.Generation) (*acceptedKeyring, error) {
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
func (t *Trust) pool() (*x509.CertPool, error) {
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
	return strings.HasPrefix(claim, cfg.IssuerSecretName+"/"+cfg.KeyringSecretName+"/")
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
	if s.NextRotation.IsZero() || s.Retiring == nil || !containsRoot(b, s.ActiveIssuer) || (s.PreparedIssuer == "") != s.ActivateAt.IsZero() {
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

	required := map[string]struct{}{}

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
			required[id] = struct{}{}
		}
	}

	prepared := map[string]bool{}

	for _, key := range b.CacheKeys {
		if key.State == wire.RetiringKey {
			required[keyID(key)] = struct{}{}
		}

		if key.State == wire.PreparedKey {
			scope := string(key.Key.Cache) + "/" + string(key.Key.Purpose)
			if s.ActivateAt.IsZero() || prepared[scope] {
				return wire.Unavailable
			}

			prepared[scope] = true
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

	if m.Pending != "" {
		if _, ok := m.Keys[m.Pending]; !ok {
			return wire.Unavailable
		}
	}

	return nil
}

// CatalogGate serializes authoritative catalog and credential operations while
// allowing callers to abandon admission when their context is canceled.
type CatalogGate struct {
	token chan struct{}
}

func newCatalogGate() *CatalogGate {
	g := &CatalogGate{token: make(chan struct{}, 1)}
	g.token <- struct{}{}

	return g
}

// Acquire returns ownership only for a live context. A failed acquisition must
// not be released and does not constitute an observation of invalid authority.
func (g *CatalogGate) Acquire(ctx context.Context) error {
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
func (g *CatalogGate) Release() {
	g.token <- struct{}{}
}
