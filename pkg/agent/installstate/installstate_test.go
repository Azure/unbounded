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

// useTempDir points the package at a scratch directory for the duration of a
// test. Not parallel-safe, which is why these tests do not call t.Parallel.
func useTempDir(t *testing.T) string {
	t.Helper()

	original := Dir
	dir := t.TempDir()
	Dir = dir

	t.Cleanup(func() { Dir = original })

	return dir
}

// completeRecord returns a record with every field validation requires, so a
// test that is not about validation does not accidentally depend on it.
func completeRecord() Record {
	return Record{
		InstallID:         "install-1",
		MachineName:       "node-1",
		HostPrefix:        "/usr/local",
		ConfigFingerprint: "fingerprint-1",
		Stage:             StageInstalling,
	}
}

func TestLoadReportsNotFoundOnCleanHost(t *testing.T) {
	useTempDir(t)

	_, err := Load()
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	useTempDir(t)

	rec := Record{
		InstallID:         "abc123",
		MachineName:       "node-1",
		HostPrefix:        "/opt/unbounded",
		ConfigFingerprint: "fp",
		Stage:             StageInstalling,
	}
	require.NoError(t, Save(rec))

	got, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "abc123", got.InstallID)
	assert.Equal(t, "/opt/unbounded", got.HostPrefix)
	assert.Equal(t, StageInstalling, got.Stage)
	assert.Equal(t, SchemaVersion, got.SchemaVersion)
	assert.False(t, got.UpdatedAt.IsZero())
}

// TestLoadRejectsUnreadableRecord covers the case that matters most: a record
// that cannot be parsed may still describe files on this host, so treating it
// as absent would let bootstrap run over the top of them.
func TestLoadRejectsUnreadableRecord(t *testing.T) {
	dir := useTempDir(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, stateFileName), []byte("{not json"), 0o600))

	_, err := Load()
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound))
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	useTempDir(t)

	require.NoError(t, Save(completeRecord()))

	// Rewrite with a future schema version.
	data := []byte(`{"schemaVersion": 99, "stage": "installing"}`)
	require.NoError(t, os.WriteFile(StatePath(), data, 0o600))

	_, err := Load()
	require.ErrorContains(t, err, "newer agent")
}

// TestMarkCompleteIsDurableAndOrdered pins that completion is only observable
// once the daemon setup it stands for has finished.
func TestMarkCompleteIsDurableAndOrdered(t *testing.T) {
	useTempDir(t)

	rec := completeRecord()
	require.NoError(t, Save(rec))

	assert.False(t, IsComplete(), "a fresh install must not look complete")

	require.NoError(t, MarkComplete(rec))
	assert.True(t, IsComplete())

	got, err := Load()
	require.NoError(t, err)
	assert.Equal(t, StageComplete, got.Stage)
}

// TestRemoveClearsMarkerAndRecord covers repeated reset: teardown has to be
// safe to run again after being interrupted.
func TestRemoveClearsMarkerAndRecord(t *testing.T) {
	useTempDir(t)

	require.NoError(t, MarkComplete(completeRecord()))
	require.NoError(t, Remove())

	assert.False(t, IsComplete())

	_, err := Load()
	require.ErrorIs(t, err, ErrNotFound)

	// Removing again must not fail.
	require.NoError(t, Remove())
}

// TestLoadRejectsIncompleteRecord covers a record that decodes but does not
// describe an installation. Decoding proves only that the bytes were JSON;
// guessing at a missing identity is how a foreign host gets adopted.
func TestLoadRejectsIncompleteRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{
			name: "no identity",
			json: `{"schemaVersion":1,"stage":"installing"}`,
			want: "missing installID",
		},
		{
			name: "no stage",
			json: `{"schemaVersion":1,"installID":"i","machineName":"m","hostPrefix":"/usr/local","configFingerprint":"f"}`,
			want: "missing stage",
		},
		{
			name: "unknown stage",
			json: `{"schemaVersion":1,"installID":"i","machineName":"m","hostPrefix":"/usr/local","configFingerprint":"f","stage":"weird"}`,
			want: "unrecognized stage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempDir(t)
			require.NoError(t, os.WriteFile(StatePath(), []byte(tc.json), 0o600))

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, errors.Is(err, ErrNotFound),
				"an unusable record must not read as an absent one")
		})
	}
}

// TestCompletionMatchesRejectsForeignMarker covers a marker left behind by an
// earlier installation. It must not vouch for a later one.
func TestCompletionMatchesRejectsForeignMarker(t *testing.T) {
	useTempDir(t)

	rec := completeRecord()
	require.NoError(t, MarkComplete(rec))

	matches, err := CompletionMatches(rec)
	require.NoError(t, err)
	assert.True(t, matches)

	other := rec
	other.InstallID = "a-different-install"

	matches, err = CompletionMatches(other)
	require.NoError(t, err)
	assert.False(t, matches, "a marker from another install must not count")
}

// TestCompletionMatchesAcceptsLegacyEmptyMarker keeps hosts marked complete
// before the ID was recorded working.
func TestCompletionMatchesAcceptsLegacyEmptyMarker(t *testing.T) {
	useTempDir(t)

	rec := completeRecord()
	require.NoError(t, Save(rec))
	require.NoError(t, os.WriteFile(CompletePath(), nil, 0o644))

	matches, err := CompletionMatches(rec)
	require.NoError(t, err)
	assert.True(t, matches)
}
