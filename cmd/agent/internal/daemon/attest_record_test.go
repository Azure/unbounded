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

func TestPersistAttestConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent", "attest.json")

	require.NoError(t, persistAttestConfigAt(discardLogger(), path, &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}).Do(context.Background()))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := readAttestRecord(path)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "http://10.0.0.1:8880", got.URL)
}

func TestPersistAttestConfigWritesNothingWithoutAttestation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attest.json")

	for _, attest := range []*provision.AgentAttestConfig{nil, {}} {
		require.NoError(t, persistAttestConfigAt(discardLogger(), path, attest).Do(context.Background()))
		assert.NoFileExists(t, path)
	}
}

func TestReadAttestRecord(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "attest.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return path
	}

	t.Run("absent", func(t *testing.T) {
		got, err := readAttestRecord(filepath.Join(t.TempDir(), "attest.json"))
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("empty URL is no record", func(t *testing.T) {
		got, err := readAttestRecord(write(t, `{"URL":""}`))
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("corrupt", func(t *testing.T) {
		_, err := readAttestRecord(write(t, `{`))
		require.ErrorContains(t, err, "decode attestation settings")
	})
}
