// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// MigrateHostRoot links the host root to the legacy root on a host installed
// by an agent released before the host root. Commands that change the host
// call it before resolving any path; see hostroot.Migrate.
func MigrateHostRoot(log *slog.Logger) error {
	return hostroot.Migrate(log, goalstates.HostRootMarkers()...)
}
