// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/x509"
	"errors"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Trust holds only controller-validated public roots. Requests never refresh it
// or fall back to Kubernetes. A failed observation cannot restore withdrawn trust.
type Trust struct {
	mu    sync.RWMutex
	roots *x509.CertPool
}

func (t *Trust) install(roots *x509.CertPool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.roots = roots
}

func (t *Trust) invalidate() {
	if t != nil {
		t.install(nil)
	}
}

// pool is immutable after installation, including when shared with TLS configs.
func (t *Trust) pool() (*x509.CertPool, error) {
	if t == nil {
		return nil, wire.Unavailable
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.roots == nil {
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

func observedAuthorityFailure(err error) bool {
	var unread authorityReadError
	return err != nil && !errors.As(err, &unread)
}
