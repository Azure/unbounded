// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

func ApplyManifests(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, manifestPath string) error {
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return fmt.Errorf("manifest not found: %w", err)
	}

	l := logger.With("kubectl_op", "apply", "manifest", manifestPath)

	return KubectlCmd(ctx, l, kubectl, "apply", "-f", manifestPath)
}
