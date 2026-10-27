// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"log/slog"
)

// prepareAgentRuntime, which runs gantry init, is a no-op on non-linux. Only
// the linux build, which is what the DaemonSet runs, re-owns the identity
// directory; non-linux builds are dev/test only.
func prepareAgentRuntime(_ string, _ *slog.Logger) error {
	return nil
}
