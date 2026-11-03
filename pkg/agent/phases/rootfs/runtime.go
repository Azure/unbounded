// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type installHostRuntime struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
	path      string
}

// InstallHostRuntime returns a task that installs the OCI runtime (runc) the
// container isolation backend uses to run the node container, at path on the
// host. It reuses the runc version and source the rootfs installers resolve, so
// a configured Runc version or download override applies to the host runtime as
// well as to the runc inside the node. The download is skipped when the
// installed runtime already reports the expected version, and an install
// replaces the file atomically so a running container's supervisor never sees
// a partial binary.
//
// This is a host artifact: it is not placed under any node root.
func InstallHostRuntime(log *slog.Logger, goalState *goalstates.RootFS, path string) phases.Task {
	return &installHostRuntime{log: log, goalState: goalState, path: path}
}

func (t *installHostRuntime) Name() string { return "install-host-runtime" }

func (t *installHostRuntime) Do(ctx context.Context) error {
	override := runcDownloadSource(t.goalState)
	version := downloadSourceVersion(t.goalState.RunCVersion, override)

	url, err := runcDownloadURL(override, version, t.goalState.HostArch)
	if err != nil {
		return fmt.Errorf("resolve host runtime download source: %w", err)
	}

	// The installed file is always named runc by the match helper, so the
	// runtime must be called runc.
	if filepath.Base(t.path) != "runc" {
		return fmt.Errorf("host runtime path %q must end in runc", t.path)
	}

	if runcVersionMatch(ctx, t.log, filepath.Dir(t.path), version) {
		t.log.Info("host runtime is already installed", "path", t.path, "version", version)

		return nil
	}

	if err := url.DownloadToLocalFile(ctx, t.path, 0o755); err != nil {
		return fmt.Errorf("download host runtime: %w", err)
	}

	return nil
}
