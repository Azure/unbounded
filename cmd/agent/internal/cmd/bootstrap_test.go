// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// These fixtures are produced by P6's actual loader, normalizer, fingerprint,
// and Store.Save. Later releases must consume them with the original input.
func TestBootstrapV1CompatibilityFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "bootstrap-v1")
	cfg, err := loadConfigFromFile(filepath.Join(dir, "input.json"))
	require.NoError(t, err)
	require.NoError(t, normalizeConfig(slog.New(slog.DiscardHandler), cfg))
	require.NoError(t, cfg.Validate())
	id, err := bootstrapIdentity(cfg)
	require.NoError(t, err)

	for _, checkpoint := range []installstate.Checkpoint{installstate.PreparingRootFS, installstate.Complete, installstate.Resetting} {
		fixture := filepath.Join(dir, string(checkpoint)+".json")
		if os.Getenv("UPDATE_BOOTSTRAP_V1_FIXTURES") == "1" {
			store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
			record, err := installstate.NewRecord(id.MachineName, id.ConfigFingerprint)
			require.NoError(t, err)

			record.InstallID = "00112233445566778899aabbccddeeff"
			record.Checkpoint = checkpoint
			require.NoError(t, store.Save(record))
			data, err := os.ReadFile(store.StatePath())
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(fixture, data, 0o644))
		}

		data, err := os.ReadFile(fixture)
		require.NoError(t, err)

		var record installstate.Record
		require.NoError(t, json.Unmarshal(data, &record))
		require.NoError(t, record.Validate())
		require.Equal(t, id.MachineName, record.MachineName)
		require.Equal(t, id.ConfigFingerprint, record.ConfigFingerprint)
		require.Equal(t, "/usr/local", record.HostPrefix)

		disposition, err := installstate.Decide(record, nil, id.MachineName, id.ConfigFingerprint)
		if checkpoint == installstate.Resetting {
			require.Error(t, err)
		} else {
			require.NoError(t, err)

			want := installstate.Resume
			if checkpoint == installstate.Complete {
				want = installstate.AlreadyComplete
			}

			require.Equal(t, want, disposition)
		}
	}

	cfg.OCIImage = "example.test/unbounded/node:changed"
	changed, err := bootstrapIdentity(cfg)
	require.NoError(t, err)
	require.NotEqual(t, id.ConfigFingerprint, changed.ConfigFingerprint)
}

func TestCompletedPreflightOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	h := &preflightHandler{writer: &out, output: "json"}
	require.NoError(t, h.writeReport(preflight.Report{}))
	require.True(t, json.Valid(out.Bytes()))

	h.output = "unsupported"
	require.Error(t, h.writeReport(preflight.Report{}))
}
