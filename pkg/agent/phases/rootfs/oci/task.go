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

	"github.com/Azure/unbounded/pkg/agent/artifactsource/ocilayout"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// RebuildPolicy says what the task may do with a machine directory that
// already has content but no completion marker.
type RebuildPolicy int

const (
	// RebuildNever refuses to touch existing content. This is the safe default
	// for any caller that has not established who owns the directory.
	//
	// A missing marker does not prove a rootfs is incomplete. Installations
	// made before the marker existed have content and no marker, and so does a
	// directory belonging to something else entirely. Deleting on that basis
	// would destroy a working node.
	RebuildNever RebuildPolicy = iota

	// RebuildOwned allows discarding and re-extracting. Callers may only pass
	// this once they have established that the directory belongs to an
	// installation of theirs that has not yet started a node.
	RebuildOwned
)

type downloadRootFS struct {
	log        *slog.Logger
	machineDir string
	ociImage   string
	hostArch   string
	rebuild    RebuildPolicy
}

// DownloadRootFS downloads an OCI image and unpacks it into the machine
// directory as rootfs.
//
// rebuild decides what happens when the directory already has content that is
// not marked complete; see RebuildPolicy.
func DownloadRootFS(
	log *slog.Logger,
	machineDir string,
	hostArch string,
	ociImage string,
	rebuild RebuildPolicy,
) phases.Task {
	return &downloadRootFS{
		log:        log,
		machineDir: machineDir,
		ociImage:   ociImage,
		hostArch:   hostArch,
		rebuild:    rebuild,
	}
}

func (d *downloadRootFS) Name() string { return "oci-download-rootfs" }

// rootfsCompleteMarker is written into the machine directory once the OCI
// layout has been fully unpacked.
//
// "Directory is not empty" cannot distinguish a finished rootfs from one whose
// extraction was interrupted, and treating the second as finished is worse than
// re-doing the first: the node then starts against a rootfs missing arbitrary
// files. The marker makes completion explicit.
const rootfsCompleteMarker = ".unbounded-rootfs-complete"

func (d *downloadRootFS) Do(ctx context.Context) error {
	empty, err := utilio.IsDirEmpty(d.machineDir)
	if err != nil {
		return fmt.Errorf("check machine directory %s: %w", d.machineDir, err)
	}

	if !empty {
		complete, err := d.rootfsComplete()
		if err != nil {
			return err
		}

		if complete {
			d.log.Info("machine directory already holds a complete rootfs, skipping bootstrap",
				slog.String("dir", d.machineDir))

			return nil
		}

		// Content with no completion marker. This is either an interrupted
		// extraction of ours, or a rootfs that predates the marker, or someone
		// else's directory. Only a caller that knows which may say.
		if d.rebuild != RebuildOwned {
			return fmt.Errorf(
				"machine directory %s already has content that is not marked complete; "+
					"it may be an installation made before completion was recorded, or one "+
					"belonging to something else, so it is left untouched. Run "+
					"'unbounded-agent reset' to clear it if it is no longer wanted",
				d.machineDir,
			)
		}

		// Owned and never started as a node, so discarding it is safe.
		d.log.Warn("discarding an incomplete rootfs from an earlier attempt and re-extracting",
			slog.String("dir", d.machineDir))

		if err := utilio.CleanDir(d.machineDir); err != nil {
			return fmt.Errorf("clear incomplete rootfs %s: %w", d.machineDir, err)
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

	if err := d.markComplete(); err != nil {
		return err
	}

	d.log.Info("OCI image extraction complete", slog.String("dest", d.machineDir))

	return nil
}

// rootfsComplete reports whether the machine directory carries the marker
// written after a successful extraction.
func (d *downloadRootFS) rootfsComplete() (bool, error) {
	markerPath := filepath.Join(d.machineDir, rootfsCompleteMarker)

	switch _, err := os.Stat(markerPath); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check rootfs completion marker %s: %w", markerPath, err)
	}
}

// markComplete records that extraction finished, and makes that durable.
//
// Without the sync the marker can reach disk before the extracted files do, so
// a crash here would leave a rootfs that claims to be complete and is not.
func (d *downloadRootFS) markComplete() error {
	if err := utilio.SyncDirTree(d.machineDir); err != nil {
		return fmt.Errorf("flush extracted rootfs %s: %w", d.machineDir, err)
	}

	markerPath := filepath.Join(d.machineDir, rootfsCompleteMarker)
	if err := utilio.WriteFile(markerPath, nil, 0o644); err != nil {
		return fmt.Errorf("mark rootfs complete: %w", err)
	}

	if err := utilio.SyncDir(d.machineDir); err != nil {
		return fmt.Errorf("flush rootfs completion marker: %w", err)
	}

	return nil
}

// CheckImageReachable validates that an OCI registry manifest, local layout,
// or HTTPS OCI layout archive is reachable without pulling image contents.
func CheckImageReachable(ctx context.Context, image string) error {
	return ocilayout.Probe(ctx, image)
}
