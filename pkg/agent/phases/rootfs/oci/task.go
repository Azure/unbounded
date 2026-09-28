// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource/ocilayout"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type downloadRootFS struct {
	log         *slog.Logger
	machineDir  string
	ociImage    string
	hostArch    string
	ownedReplay bool
}

// DownloadRootFS downloads an OCI image and unpacks it into the machine
// directory as rootfs.
func DownloadRootFS(
	log *slog.Logger,
	machineDir string,
	hostArch string,
	ociImage string,
) phases.Task {
	return &downloadRootFS{
		log:        log,
		machineDir: machineDir,
		ociImage:   ociImage,
		hostArch:   hostArch,
	}
}

func (d *downloadRootFS) Name() string { return "oci-download-rootfs" }

// DownloadOwnedRootFS is for owned initial installation only. The caller must
// hold installation ownership and prove this slot has never started a node.
// The original DownloadRootFS entry point keeps its existing nonempty-rootfs
// behavior for callers managing legacy installations.
func DownloadOwnedRootFS(log *slog.Logger, machineDir, hostArch, image string) phases.Task {
	return &downloadRootFS{log: log, machineDir: machineDir, hostArch: hostArch, ociImage: image, ownedReplay: true}
}

func (d *downloadRootFS) Do(ctx context.Context) error {
	empty, err := utilio.IsDirEmpty(d.machineDir)
	if err != nil {
		return fmt.Errorf("check machine directory %s: %w", d.machineDir, err)
	}

	if !empty {
		if !d.ownedReplay {
			d.log.Warn("machine directory is not empty, skipping rootfs bootstrap", slog.String("dir", d.machineDir))
			return nil
		}

		if _, err := os.Stat(filepath.Join(d.machineDir, ".unbounded-rootfs-complete")); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := utilio.CleanDir(d.machineDir); err != nil {
			return err
		}
	}

	d.log.Info("acquiring OCI image",
		slog.String("image", utilio.RedactURLQuery(d.ociImage)),
		slog.String("dest", d.machineDir))

	layout, err := ocilayout.Acquire(ctx, d.ociImage)
	if err != nil {
		return fmt.Errorf("acquire OCI image: %w", err)
	}
	defer layout.Close() //nolint:errcheck // best effort cleanup

	if err := os.MkdirAll(d.machineDir, 0o755); err != nil {
		return fmt.Errorf("create machine directory: %w", err)
	}

	if err := unpackOCILayout(ctx, d.log, d.hostArch, layout.Dir, layout.Reference, d.machineDir); err != nil {
		return fmt.Errorf("unpack OCI image: %w", err)
	}

	if d.ownedReplay {
		if err := fsutil.SyncFilesystems(d.machineDir); err != nil {
			return err
		}

		if err := fsutil.WriteFileDurable(filepath.Join(d.machineDir, ".unbounded-rootfs-complete"), []byte("complete\n"), 0o600); err != nil {
			return err
		}
	}

	d.log.Info("OCI image extraction complete", slog.String("dest", d.machineDir))

	return nil
}

// CheckImageReachable validates that an OCI registry manifest, local layout,
// or HTTPS OCI layout archive is reachable without pulling image contents.
func CheckImageReachable(ctx context.Context, image string) error {
	return ocilayout.Probe(ctx, image)
}
