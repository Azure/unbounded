// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// TestBootstrapV1CompatibilityFixtures pins the on-disk ownership format at
// schema version 1. The fixtures hold records written by this package's own
// config loader, normalizer, fingerprint and installstate.Store, from the
// synthetic input.json alongside them; only the random install ID is fixed.
//
// A later release must still admit an installation created by an earlier one
// when given the same original input, so a mismatch here is a compatibility
// break rather than a fixture to refresh. Record.Validate pins these files to
// the package's schema version, so bumping it fails loudly; a new schema version
// gets its own fixture directory rather than regenerated files.
func TestBootstrapV1CompatibilityFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "bootstrap-v1")
	cfg, err := loadConfigFromFile(filepath.Join(dir, "input.json"))
	require.NoError(t, err)
	require.NoError(t, normalizeConfig(slog.New(slog.DiscardHandler), cfg))
	require.NoError(t, cfg.Validate())
	id, err := bootstrapIdentity(cfg)
	require.NoError(t, err)

	for _, phase := range []installstate.Phase{installstate.Installing, installstate.Complete, installstate.Resetting} {
		data, err := os.ReadFile(filepath.Join(dir, string(phase)+".json"))
		require.NoError(t, err)

		var record installstate.Record
		require.NoError(t, json.Unmarshal(data, &record))
		require.NoError(t, record.Validate())
		require.Equal(t, id.MachineName, record.MachineName)
		require.Equal(t, id.ConfigFingerprint, record.ConfigFingerprint)

		// Admit through a store so the fixture also proves it survives a
		// load round-trip, not just an in-memory classification.
		store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
		require.NoError(t, store.Save(record))

		loaded, disposition, err := installstate.Admit(store, id.MachineName, id.ConfigFingerprint)
		if phase == installstate.Resetting {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, record, loaded)

			want := installstate.Resume
			if phase == installstate.Complete {
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

func TestBootstrapFingerprintAllowsCredentialAndDownloadRefresh(t *testing.T) {
	cfg, err := loadConfigFromFile(filepath.Join("testdata", "bootstrap-v1", "input.json"))
	require.NoError(t, err)
	original, err := bootstrapIdentity(cfg)
	require.NoError(t, err)

	cfg.Kubelet.Auth.BootstrapToken = "rotated-token"
	cfg.Cluster.CaCertBase64 = "rotated-ca"
	cfg.Downloads = &provision.AgentDownloads{Kubernetes: &provision.AgentDownloadSource{BaseURL: "https://mirror.example.test"}}
	refreshed, err := bootstrapIdentity(cfg)
	require.NoError(t, err)
	require.Equal(t, original, refreshed)

	for _, change := range []func(){
		func() { cfg.Cluster.Version = "1.35.0" },
		func() { cfg.OCIImage = "other-image" },
		func() { cfg.Kubelet.ApiServer = "https://other-cluster" },
	} {
		version, image, endpoint := cfg.Cluster.Version, cfg.OCIImage, cfg.Kubelet.ApiServer

		change()

		changed, err := bootstrapIdentity(cfg)
		require.NoError(t, err)
		require.NotEqual(t, original.ConfigFingerprint, changed.ConfigFingerprint)

		cfg.Cluster.Version, cfg.OCIImage, cfg.Kubelet.ApiServer = version, image, endpoint
	}
}

func TestBootstrapFingerprintIgnoresSignedImageQuery(t *testing.T) {
	t.Parallel()

	// A signed archive URL carries an expiring signature. Refreshing it points
	// at the same artifact, so a retry must stay the same installation rather
	// than being rejected as a different one.
	const base = "https://artifacts.example.test/node/rootfs.oci.tar.gz"

	cfg, err := loadConfigFromFile(filepath.Join("testdata", "bootstrap-v1", "input.json"))
	require.NoError(t, err)

	cfg.OCIImage = base + "?sp=r&sv=2022-11-02&sig=first-signature"
	original, err := bootstrapIdentity(cfg)
	require.NoError(t, err)

	for _, equivalent := range []string{
		base + "?sp=r&sv=2022-11-02&sig=second-signature",
		base + "?",
		base + "/",
		base,
	} {
		cfg.OCIImage = equivalent

		refreshed, err := bootstrapIdentity(cfg)
		require.NoError(t, err)
		require.Equal(t, original.ConfigFingerprint, refreshed.ConfigFingerprint, "reference %q", equivalent)
	}

	// The path and host still identify the artifact, so they must not be
	// collapsed away with the credential.
	for _, different := range []string{
		"https://artifacts.example.test/node/other-rootfs.oci.tar.gz",
		"https://other-host.example.test/node/rootfs.oci.tar.gz",
	} {
		cfg.OCIImage = different

		changed, err := bootstrapIdentity(cfg)
		require.NoError(t, err)
		require.NotEqual(t, original.ConfigFingerprint, changed.ConfigFingerprint, "reference %q", different)
	}
}

func TestCanonicalImageIdentityLeavesNonHTTPSReferencesAlone(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		"example.test/unbounded/node:v1.33.1",
		"example.test/unbounded/node@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"oci-layout:///var/lib/unbounded/layouts/node",
		"",
	} {
		require.Equal(t, image, canonicalImageIdentity(image))
	}

	// An unparseable HTTPS reference is rejected later with a better message.
	// Identity just has to stay deterministic rather than panic.
	const malformed = "https://artifacts.example.test/\x7f"
	require.Equal(t, malformed, canonicalImageIdentity(malformed))
}

// TestNodeStartPersistsAppliedConfig pins where the applied config is written.
// It has to happen in the stage that starts the node, and after kubelet has
// bootstrapped, so the record always describes the configuration the running
// node was built from. Moving it into the daemon stage would let a retry that
// resumes there record a configuration the node never saw, which then reads as
// "no drift" and is never reconciled.
func TestNodeStartPersistsAppliedConfig(t *testing.T) {
	t.Parallel()

	stages := &agentStages{
		log: slog.New(slog.DiscardHandler),
		cfg: &provision.UnboundedAgentConfig{},
		gs: &goalstates.MachineGoalState{
			NodeStart: &goalstates.NodeStart{MachineName: goalstates.NSpawnMachineKube1},
		},
	}

	nodeStart := stages.nodeStartTask().Name()
	require.Contains(t, nodeStart, "persist-applied-config")
	require.Less(t, strings.Index(nodeStart, "wait-for-kubelet-bootstrap"), strings.Index(nodeStart, "persist-applied-config"))

	require.NotContains(t, stages.daemonInstallTask().Name(), "persist-applied-config")
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

// TestClassifyNodeStartFailure pins the Machine condition reasons for a node
// that fails to come up.
//
// wait-for-kubelet-bootstrap has to map to KubeletBootstrapFailed alongside
// start-kubelet. It is its own task inside the node-start stage, and a failure
// there is the most common real one: a rejected or expired bootstrap token, an
// unreachable API server, a CA mismatch. Reporting it as a generic failure
// tells an operator nothing about where to look.
func TestClassifyNodeStartFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ task, want string }{
		{"start-kubelet", "KubeletBootstrapFailed"},
		{"wait-for-kubelet-bootstrap", "KubeletBootstrapFailed"},
		{"start-nspawn-machine", "NSpawnFailed"},
		{"import-container-images", "Failed"},
	} {
		t.Run(tc.task, func(t *testing.T) {
			t.Parallel()

			err := fmt.Errorf("%s: %w", tc.task, errors.New("boom"))
			require.Equal(t, tc.want, classifyNodeStartFailure(err))
		})
	}
}
