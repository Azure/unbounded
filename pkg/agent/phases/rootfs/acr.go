// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type downloadACRProvider struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// DownloadACRProvider returns a task that downloads the stock ACR credential
// provider binary into the node stack's binary directory. It is verified
// against the SHA256 pinned in agentartifacts for the architecture, so a
// mirrored or tampered artifact is rejected. The task is a no-op when the
// binary is already present and executable: it is a fixed upstream release,
// not versioned with the cluster, so there is nothing to re-check.
func DownloadACRProvider(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadACRProvider{log: log, goalState: goalState}
}

func (d *downloadACRProvider) Name() string { return "download-acr-provider" }

func (d *downloadACRProvider) Do(ctx context.Context) error {
	destDir := d.goalState.Root().Path(goalstates.RegistryAuthProviderBinDir)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", destDir, err)
	}

	target := filepath.Join(destDir, goalstates.RegistryAuthProviderName)

	if utilio.IsExecutable(target) {
		return nil
	}

	arch := d.goalState.HostArch

	expectedHash, err := agentartifacts.ACRProviderSHA256For(arch)
	if err != nil {
		return fmt.Errorf("resolve ACR provider hash: %w", err)
	}

	binaryURL, err := acrProviderBinaryURL(acrProviderDownloadSource(d.goalState), arch)
	if err != nil {
		return fmt.Errorf("resolve ACR provider download source: %w", err)
	}

	logger := d.log.With("binary", goalstates.RegistryAuthProviderName)
	logger.Info("downloading ACR credential provider")

	start := time.Now()

	if err := binaryURL.DownloadWithSHA256Verification(ctx, expectedHash, target, 0o755); err != nil {
		logger.Error("download failed", "error", err)
		return fmt.Errorf("download ACR credential provider %q: %w", target, err)
	}

	logger.Info("downloaded ACR credential provider", "duration", time.Since(start))

	return nil
}

// acrProviderBinaryURL parses the resolved ACR provider download URL into an
// artifact source.
func acrProviderBinaryURL(source *goalstates.DownloadSource, arch string) (artifactsource.Source, error) {
	url := agentartifacts.ACRProviderBinary(source, arch)

	return artifactsource.Parse(url)
}
