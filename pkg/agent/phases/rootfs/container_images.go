// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type downloadContainerImageArchives struct {
	logState *goalstates.RootFS
}

// DownloadContainerImageArchives returns a task that downloads container image
// archives into the nspawn rootfs before the machine starts. The running node
// imports these archives into containerd during node-start.
func DownloadContainerImageArchives(_ *slog.Logger, gs *goalstates.RootFS) phases.Task {
	return &downloadContainerImageArchives{logState: gs}
}

func (d *downloadContainerImageArchives) Name() string { return "download-container-image-archives" }

func (d *downloadContainerImageArchives) Do(ctx context.Context) error {
	if d.logState == nil {
		return fmt.Errorf("rootfs goal state is required")
	}

	for idx, archiveURL := range d.logState.ContainerImageArchiveURLs {
		archivePath := goalstates.ContainerImageArchivePath(idx)
		hostPath := filepath.Join(d.logState.MachineDir, strings.TrimPrefix(archivePath, "/"))

		if err := downloadWithSHA256Verification(ctx, archiveURL, archiveURL+".sha256", hostPath, 0o644); err != nil {
			return fmt.Errorf("download container image archive %q: %w", archiveURL, err)
		}
	}

	return nil
}
