// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CheckServiceDeactivationSafe refuses to stop host services from a container.
// The container may depend on the very runtime being stopped.
func CheckServiceDeactivationSafe(ctx context.Context) error {
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("inspect agent cgroup: %w", err)
	}

	err = exec.CommandContext(ctx, "systemd-detect-virt", "--quiet", "--container").Run()

	return checkServiceDeactivationSafe(string(cgroup), err)
}

func checkServiceDeactivationSafe(cgroup string, detectErr error) error {
	for _, marker := range []string{"docker", "containerd", "kubepods", "libpod"} {
		if strings.Contains(strings.ToLower(cgroup), marker) {
			return errors.New("cannot deactivate host services from a container; run the agent on the host")
		}
	}

	if detectErr == nil {
		return errors.New("cannot deactivate host services from a container; run the agent on the host")
	}

	var exitErr *exec.ExitError
	if !errors.As(detectErr, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("detect container execution: %w", detectErr)
	}

	return nil
}
