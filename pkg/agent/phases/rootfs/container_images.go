// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type downloadContainerImageArchives struct {
	staging *goalstates.ContainerImageArchiveStaging
}

// DownloadContainerImageArchives returns a task that downloads container image
// archives into a host-side staging directory. The nspawn machine bind-mounts
// this directory read-only and imports these archives into containerd during
// node-start.
func DownloadContainerImageArchives(_ *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) phases.Task {
	return &downloadContainerImageArchives{staging: staging}
}

func (d *downloadContainerImageArchives) Name() string { return "download-container-image-archives" }

func (d *downloadContainerImageArchives) Do(ctx context.Context) error {
	if d.staging == nil {
		return fmt.Errorf("container image archive staging is required")
	}

	if d.staging.HostDir == "" {
		return fmt.Errorf("container image archive host directory is required")
	}

	if err := os.MkdirAll(d.staging.HostDir, 0o750); err != nil {
		return fmt.Errorf("create container image archive staging directory %s: %w", d.staging.HostDir, err)
	}

	expected := make(map[string]struct{}, len(d.staging.URLs))
	for idx, archiveURL := range d.staging.URLs {
		archivePath := filepath.Join(d.staging.HostDir, fmt.Sprintf("image-%d.tar", idx))
		expected[archivePath] = struct{}{}

		if err := downloadWithSHA256Verification(ctx, archiveURL, archiveURL+".sha256", archivePath, 0o644); err != nil {
			return fmt.Errorf("download container image archive %q: %w", archiveURL, err)
		}
	}

	if err := removeUnexpectedContainerImageArchives(d.staging.HostDir, expected); err != nil {
		return err
	}

	if err := utilio.UpdateSymlink(goalstates.ContainerImageArchiveHostDir, d.staging.HostDir); err != nil {
		return fmt.Errorf("update container image archive symlink: %w", err)
	}

	if err := removeOtherContainerImageArchiveSources(d.staging.HostDir); err != nil {
		return err
	}

	return nil
}

func removeUnexpectedContainerImageArchives(hostDir string, expected map[string]struct{}) error {
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return fmt.Errorf("read container image archive staging directory %s: %w", hostDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tar" {
			continue
		}

		path := filepath.Join(hostDir, entry.Name())
		if _, ok := expected[path]; ok {
			continue
		}

		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale container image archive %s: %w", path, err)
		}
	}

	return nil
}

func removeOtherContainerImageArchiveSources(hostDir string) error {
	stagingRoot := filepath.Dir(hostDir)
	keep := filepath.Base(hostDir)

	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		return fmt.Errorf("read container image archive staging root %s: %w", stagingRoot, err)
	}

	for _, entry := range entries {
		if entry.Name() == keep || entry.Name() == filepath.Base(goalstates.ContainerImageArchiveHostDir) {
			continue
		}

		path := filepath.Join(stagingRoot, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove stale container image archive source %s: %w", path, err)
		}
	}

	return nil
}
