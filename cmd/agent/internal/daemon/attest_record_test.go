// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
)

func useAttestRecord(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent", "attest.json")

	old := attestRecordPath
	attestRecordPath = func() string { return path }

	t.Cleanup(func() { attestRecordPath = old })

	return path
}

func TestPersistAttestConfigRoundTrips(t *testing.T) {
	path := useAttestRecord(t)

	require.NoError(t, PersistAttestConfig(discardLogger(), &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}).Do(context.Background()))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := readAttestRecord()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "http://10.0.0.1:8880", got.URL)
}

func TestPersistAttestConfigWritesNothingWithoutAttestation(t *testing.T) {
	path := useAttestRecord(t)

	for _, attest := range []*provision.AgentAttestConfig{nil, {}} {
		require.NoError(t, PersistAttestConfig(discardLogger(), attest).Do(context.Background()))
		assert.NoFileExists(t, path)
	}
}

func TestReadAttestRecord(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		useAttestRecord(t)

		got, err := readAttestRecord()
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("empty URL is no record", func(t *testing.T) {
		path := useAttestRecord(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(`{"URL":""}`), 0o600))

		got, err := readAttestRecord()
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("corrupt", func(t *testing.T) {
		path := useAttestRecord(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))

		_, err := readAttestRecord()
		require.ErrorContains(t, err, "decode attestation settings")
	})
}
