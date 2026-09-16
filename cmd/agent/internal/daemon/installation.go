// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func installationStore(store *installstate.Store) *installstate.Store {
	if store != nil {
		return store
	}

	return installstate.DefaultStore()
}

func releaseInstallationLock(log *slog.Logger, lock *installstate.Lock) {
	if err := lock.Release(); err != nil {
		log.Error("release installation lock", "error", err)
	}
}
