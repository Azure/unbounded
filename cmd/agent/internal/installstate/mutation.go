// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"fmt"
)

// AcquireMutationLock serializes ordinary lifecycle work with bootstrap and
// reset. Legacy hosts without a record remain supported. When both locks are
// needed, acquire installation ownership before the binary activation lock.
func (s *Store) AcquireMutationLock() (*Lock, error) {
	lock, err := s.AcquireLock()
	if err != nil {
		return nil, err
	}

	r, loadErr := s.Load()
	if errors.Is(loadErr, ErrNotFound) || loadErr == nil && r.Checkpoint == Complete {
		return lock, nil
	}

	closeErr := lock.Release()

	if loadErr != nil {
		return nil, errors.Join(loadErr, closeErr)
	}

	return nil, errors.Join(fmt.Errorf("installation is %s; finish bootstrap or reset before lifecycle operations", r.Checkpoint), closeErr)
}
