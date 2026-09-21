// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"fmt"
)

// ErrInstallationInProgress reports that an installation owns this host and has
// not finished. It is distinct from ErrLockHeld, which says another process is
// working right now: this one says the host is mid-installation whether or not
// anyone is currently acting on it.
//
// Callers that run because systemd started them, rather than because a person
// asked, must not treat this as a failure. An installation in progress is an
// ordinary state, and reporting it as a crash is how a service ends up being
// restarted, rate-limited, and recovered for a problem it does not have.
var ErrInstallationInProgress = errors.New("installation has not finished")

// AcquireMutationLock serializes ordinary lifecycle work with bootstrap and
// reset. Legacy hosts without a record remain supported. When both locks are
// needed, acquire installation ownership before the binary activation lock.
func (s *Store) AcquireMutationLock() (*Lock, error) {
	lock, err := s.AcquireLock()
	if err != nil {
		return nil, err
	}

	r, loadErr := s.Load()
	if errors.Is(loadErr, ErrNotFound) || loadErr == nil && r.Phase == Complete {
		return lock, nil
	}

	closeErr := lock.Release()

	if loadErr != nil {
		return nil, errors.Join(loadErr, closeErr)
	}

	return nil, errors.Join(fmt.Errorf("%w: installation is %s; finish bootstrap or reset before lifecycle operations", ErrInstallationInProgress, r.Phase), closeErr)
}
