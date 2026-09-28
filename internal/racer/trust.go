// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

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

func (t *Trust) invalidate() {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()

		t.roots, t.bundle = nil, nil
		t.notifyLocked()
	}
}

func (t *Trust) keyring() (*acceptedKeyring, <-chan struct{}, error) {
	if t == nil {
		return nil, nil, wire.Unavailable
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.roots == nil || t.bundle == nil {
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
