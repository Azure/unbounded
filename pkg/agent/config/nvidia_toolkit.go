// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// NvidiaToolkitConfig locates the NVIDIA Container Toolkit tools on a host that
// installs them somewhere other than /usr/bin, for example
// /usr/local/nvidia/toolkit. An empty field keeps its default.
type NvidiaToolkitConfig struct {
	// CTKPath is the nvidia-ctk executable.
	CTKPath string `json:"CTKPath,omitempty"`
	// ContainerRuntimePath is the nvidia-container-runtime executable that
	// containerd's nvidia runtime handler runs.
	ContainerRuntimePath string `json:"ContainerRuntimePath,omitempty"`
}

func (a *AgentConfig) validateNvidiaToolkit() error {
	t := a.NvidiaToolkit
	if t == nil {
		return nil
	}

	var errs []error

	if mode, err := a.IsolationMode(); err == nil && mode != isolation.ModeNone {
		errs = append(errs, fmt.Errorf("NvidiaToolkit is only supported with Isolation %q: the %s mode runs the toolkit from its rootfs image", isolation.ModeNone, mode))
	}

	for name, p := range map[string]string{"CTKPath": t.CTKPath, "ContainerRuntimePath": t.ContainerRuntimePath} {
		if err := validateToolPath(p); err != nil {
			errs = append(errs, fmt.Errorf("invalid NvidiaToolkit.%s: %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// validateToolPath accepts an empty path or a clean absolute one. A path that
// needs cleaning is refused rather than rewritten, so the value the operator
// wrote is the value the agent runs.
func validateToolPath(p string) error {
	if p == "" {
		return nil
	}

	if !path.IsAbs(p) {
		return fmt.Errorf("%q is not an absolute path", p)
	}

	if strings.Contains(p, "..") {
		return fmt.Errorf("%q must not contain \"..\"", p)
	}

	if path.Clean(p) != p {
		return fmt.Errorf("%q is not a clean path (want %q)", p, path.Clean(p))
	}

	if p == "/" {
		return fmt.Errorf("%q is not an executable", p)
	}

	return nil
}
