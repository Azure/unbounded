// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
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
	require.Equal(t, Complete, loaded.Checkpoint)
	require.NoError(t, s.Remove())
	_, err = s.Load()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestOwnershipAdmission(t *testing.T) {
	t.Parallel()

	r, err := NewRecord("machine", "fingerprint")
	require.NoError(t, err)

	for _, checkpoint := range []Checkpoint{PreparingHost, PreparingRootFS, StartingNode, InstallingDaemon, Complete, Resetting} {
		t.Run(string(checkpoint), func(t *testing.T) {
			r := r
			r.Checkpoint = checkpoint

			disposition, err := decide(r, nil, r.MachineName, r.ConfigFingerprint)
			if checkpoint == Resetting {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			want := Resume
			if checkpoint == Complete {
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

	for _, data := range []string{"{", "null", `{}`, `{"schemaVersion":2}`, `{"schemaVersion":1,"installID":"id","machineName":"machine","configFingerprint":"f","hostPrefix":"/opt/unbounded","checkpoint":"complete"}`} {
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

func TestMutationAdmission(t *testing.T) {
	t.Parallel()

	for _, checkpoint := range []Checkpoint{"", PreparingHost, StartingNode, Complete, Resetting} {
		t.Run(string(checkpoint), func(t *testing.T) {
			s := testStore(t)

			if checkpoint != "" {
				r, err := NewRecord("machine", "f")
				require.NoError(t, err)

				r.Checkpoint = checkpoint
				require.NoError(t, s.Save(r))
			}

			lock, err := s.AcquireMutationLock()
			if checkpoint == "" || checkpoint == Complete {
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
