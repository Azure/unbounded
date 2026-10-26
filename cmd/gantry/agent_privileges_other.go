// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/config"
)

// prepareAgentRuntime is a no-op on non-linux. Only the linux agent, which is
// what the DaemonSet runs, re-owns its identity directory and drops root;
// non-linux builds are dev/test only.
func prepareAgentRuntime(_ *config.Config, _ *slog.Logger) error {
	return nil
}
