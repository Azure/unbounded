// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()

	return NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "install.lock"))
}

func TestStoreLifecycle(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	require.NoError(t, s.Remove())
	_, err := s.Load()
	require.ErrorIs(t, err, ErrNotFound)
	r, err := NewRecord("machine", Fingerprint([]byte(`{"machineName":"machine"}`)))
	require.NoError(t, err)
	require.NoError(t, s.Save(r))
	loaded, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, r, loaded)

	info, err := os.Stat(s.statePath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, s.MarkComplete(r))
	loaded, err = s.Load()
	require.NoError(t, err)
	require.Equal(t, Complete, loaded.Phase)
	require.NoError(t, s.Remove())
	_, err = s.Load()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestOwnershipAdmission(t *testing.T) {
	t.Parallel()

	r, err := NewRecord("machine", "fingerprint")
	require.NoError(t, err)

	for _, phase := range []Phase{Installing, Complete, Resetting} {
		t.Run(string(phase), func(t *testing.T) {
			r := r
			r.Phase = phase

			disposition, err := decide(r, nil, r.MachineName, r.ConfigFingerprint)
			if phase == Resetting {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			want := Resume
			if phase == Complete {
				want = AlreadyComplete
			}

			require.Equal(t, want, disposition)

			_, err = decide(r, nil, "other", r.ConfigFingerprint)
			require.Error(t, err)
			_, err = decide(r, nil, r.MachineName, "other")
			require.Error(t, err)
		})
	}

	_, err = decide(Record{}, ErrNotFound, "", "")
	require.Error(t, err)
}

func TestStoreRejectsCorruptAndOrphanedOwnership(t *testing.T) {
	t.Parallel()

	for _, data := range []string{"{", "null", `{}`, `{"schemaVersion":2}`, `{"schemaVersion":1,"installID":"id","machineName":"machine","configFingerprint":"f","checkpoint":"bogus"}`} {
		t.Run(data, func(t *testing.T) {
			s := testStore(t)
			require.NoError(t, os.MkdirAll(s.Root(), 0o755))
			require.NoError(t, os.WriteFile(s.statePath(), []byte(data), 0o600))
			r, err := s.Load()
			require.Error(t, err)
			_, err = decide(r, err, "machine", "f")
			require.Error(t, err)
		})
	}
}

func TestInstallationLockSurvivesStateRemoval(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	lock, err := s.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	r, err := NewRecord("machine", "f")
	require.NoError(t, err)
	require.NoError(t, s.Save(r))
	require.NoError(t, s.Remove())
	_, err = s.AcquireLock()
	require.ErrorIs(t, err, ErrLockHeld)
	require.NoError(t, lock.Release())

	second, err := s.AcquireLock()
	require.NoError(t, err)
	require.NoError(t, second.Release())
}

// TestRemoveRestoresOwnershipWhenUndurable covers the window where the record
// is unlinked but the directory entry never reaches disk. Reporting the error
// while leaving the removal in page cache would let the next start be admitted
// as a fresh install onto a host that was only partially torn down.
func TestRemoveRestoresOwnershipWhenUndurable(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	r, err := NewRecord("machine", "f")
	require.NoError(t, err)

	r.Phase = Resetting
	require.NoError(t, s.Save(r))

	failure := errors.New("sync failed")
	s.syncDir = func(string) error { return failure }

	require.ErrorIs(t, s.Remove(), failure)

	restored, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, r, restored)

	// Reset stays incomplete, so a retry is refused rather than admitted fresh.
	_, _, err = Admit(s, r.MachineName, r.ConfigFingerprint)
	require.Error(t, err)

	// Once the removal can be made durable, ownership is released.
	s.syncDir = func(string) error { return nil }
	require.NoError(t, s.Remove())
	_, err = s.Load()
	require.ErrorIs(t, err, ErrNotFound)
}

// A record that cannot be loaded grants no usable ownership, so a failed
// removal reports the error without resurrecting it.
func TestRemoveDoesNotRestoreUnusableOwnership(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	require.NoError(t, os.MkdirAll(s.Root(), 0o755))
	require.NoError(t, os.WriteFile(s.statePath(), []byte("{"), 0o600))

	failure := errors.New("sync failed")
	s.syncDir = func(string) error { return failure }

	require.ErrorIs(t, s.Remove(), failure)
	_, err := s.Load()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMutationAdmission(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{"", Installing, Complete, Resetting} {
		t.Run(string(phase), func(t *testing.T) {
			s := testStore(t)

			if phase != "" {
				r, err := NewRecord("machine", "f")
				require.NoError(t, err)

				r.Phase = phase
				require.NoError(t, s.Save(r))
			}

			lock, err := s.AcquireMutationLock()
			if phase == "" || phase == Complete {
				require.NoError(t, err)
				_, err = s.AcquireMutationLock()
				require.ErrorIs(t, err, ErrLockHeld)
				require.NoError(t, lock.Release())
			} else {
				require.Error(t, err)
			}

			lock, err = s.AcquireLock()
			require.NoError(t, err, "failed admission must release the lock for reset")
			require.NoError(t, lock.Release())
		})
	}
}
