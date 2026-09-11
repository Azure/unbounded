// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// validRecord returns a record with every field validation requires, so a test
// that is not about validation does not accidentally depend on it.
func validRecord() Record {
	return Record{
		InstallID:         "install-1",
		MachineName:       "node-1",
		HostPrefix:        "/usr/local",
		ConfigFingerprint: "fingerprint-1",
		Checkpoint:        CheckpointPreparingHost,
	}
}

func TestLoadReportsNotFoundOnCleanHost(t *testing.T) {
	t.Parallel()

	_, err := newTestStore(t).Load()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	require.NoError(t, store.Save(validRecord()))

	got, err := store.Load()
	require.NoError(t, err)

	assert.Equal(t, "install-1", got.InstallID)
	assert.Equal(t, "/usr/local", got.HostPrefix)
	assert.Equal(t, CheckpointPreparingHost, got.Checkpoint)
	assert.Equal(t, SchemaVersion, got.SchemaVersion)
	assert.False(t, got.UpdatedAt.IsZero())
}

// TestLoadRejectsUnusableRecords covers records that decode but do not describe
// an installation. Decoding proves only that the bytes were JSON, and guessing
// at a missing field is how a foreign or half-erased host gets adopted.
func TestLoadRejectsUnusableRecords(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "not json", body: "{not json", want: "parse"},
		{
			name: "no identity",
			body: `{"schemaVersion":1,"checkpoint":"preparing-host"}`,
			want: "missing installID",
		},
		{
			name: "no checkpoint",
			body: `{"schemaVersion":1,"installID":"i","machineName":"m","hostPrefix":"/usr/local","configFingerprint":"f"}`,
			want: "missing checkpoint",
		},
		{
			name: "unknown checkpoint",
			body: `{"schemaVersion":1,"installID":"i","machineName":"m","hostPrefix":"/usr/local","configFingerprint":"f","checkpoint":"weird"}`,
			want: "unrecognized checkpoint",
		},
		{
			name: "newer schema",
			body: `{"schemaVersion":99,"installID":"i","machineName":"m","hostPrefix":"/usr/local","configFingerprint":"f","checkpoint":"complete"}`,
			want: "newer agent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			require.NoError(t, os.WriteFile(store.StatePath(), []byte(tc.body), 0o600))

			_, err := store.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, errors.Is(err, ErrNotFound),
				"an unusable record must not read as an absent one")
		})
	}
}

// TestAdvanceRefusesToGoBackwards is the safety rail on the checkpoint itself.
// Moving back past StartingNode would re-enter stages that rebuild the rootfs
// or re-flush the firewall, under a node that may be running.
func TestAdvanceRefusesToGoBackwards(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	rec := validRecord()
	rec.Checkpoint = CheckpointStartingNode
	require.NoError(t, store.Save(rec))

	_, err := store.Advance(rec, CheckpointPreparingRootFS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a node may already be running")

	// Forward is fine.
	updated, err := store.Advance(rec, CheckpointInstallingDaemon)
	require.NoError(t, err)
	assert.Equal(t, CheckpointInstallingDaemon, updated.Checkpoint)
}

func TestAdvanceRejectsUnknownCheckpoint(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	_, err := store.Advance(validRecord(), Checkpoint("nonsense"))
	require.ErrorContains(t, err, "unknown checkpoint")
}

// TestMarkCompleteIsOrdered pins that completion is only observable once the
// record behind it says so.
func TestMarkCompleteIsOrdered(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	rec := validRecord()
	require.NoError(t, store.Save(rec))

	matches, err := store.CompletionMatches(rec)
	require.NoError(t, err)
	assert.False(t, matches, "a fresh install must not look complete")

	updated, err := store.MarkComplete(rec)
	require.NoError(t, err)
	assert.Equal(t, CheckpointComplete, updated.Checkpoint)

	matches, err = store.CompletionMatches(updated)
	require.NoError(t, err)
	assert.True(t, matches)
}

// TestCompletionMatchesRejectsForeignMarker covers a marker left behind by an
// earlier installation. It must not vouch for a later one.
func TestCompletionMatchesRejectsForeignMarker(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	rec, err := store.MarkComplete(validRecord())
	require.NoError(t, err)

	other := rec
	other.InstallID = "a-different-install"

	matches, err := store.CompletionMatches(other)
	require.NoError(t, err)
	assert.False(t, matches)
}

// TestCompletionMatchesAcceptsLegacyEmptyMarker keeps hosts marked complete
// before the ID was recorded working.
func TestCompletionMatchesAcceptsLegacyEmptyMarker(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	rec := validRecord()
	require.NoError(t, store.Save(rec))
	require.NoError(t, os.WriteFile(store.CompletePath(), nil, 0o644))

	matches, err := store.CompletionMatches(rec)
	require.NoError(t, err)
	assert.True(t, matches)
}

// TestRemoveIsRepeatable covers reset being run again after an interruption.
func TestRemoveIsRepeatable(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	_, err := store.MarkComplete(validRecord())
	require.NoError(t, err)

	require.NoError(t, store.Remove())

	_, err = store.Load()
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.Remove())
}

func TestNodeMayBeRunning(t *testing.T) {
	t.Parallel()

	// Before the node starts, recovery may rebuild.
	assert.False(t, CheckpointPreparingHost.NodeMayBeRunning())
	assert.False(t, CheckpointPreparingRootFS.NodeMayBeRunning())

	// From here on it must not.
	assert.True(t, CheckpointStartingNode.NodeMayBeRunning())
	assert.True(t, CheckpointInstallingDaemon.NodeMayBeRunning())
	assert.True(t, CheckpointComplete.NodeMayBeRunning())
}

func TestFingerprintDistinguishesConfigs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Fingerprint([]byte(`{"a":1}`)), Fingerprint([]byte(`{"a":1}`)))
	assert.NotEqual(t, Fingerprint([]byte(`{"a":1}`)), Fingerprint([]byte(`{"a":2}`)))
}

func TestNewInstallIDIsUnique(t *testing.T) {
	t.Parallel()

	first, err := NewInstallID()
	require.NoError(t, err)

	second, err := NewInstallID()
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.Len(t, first, 32)
}

// TestLockIsExclusive covers bootstrap racing reset. The bootstrap unit retries
// on a timer, so overlap is a real possibility rather than a theoretical one.
func TestLockIsExclusive(t *testing.T) {
	original := LockPathForTest
	LockPathForTest = filepath.Join(t.TempDir(), "install.lock")

	t.Cleanup(func() { LockPathForTest = original })

	first, err := AcquireLock()
	require.NoError(t, err)

	_, err = AcquireLock()
	require.ErrorIs(t, err, ErrLockHeld)

	require.NoError(t, first.Release())

	// Released, so the next caller gets it.
	second, err := AcquireLock()
	require.NoError(t, err)
	require.NoError(t, second.Release())
}
