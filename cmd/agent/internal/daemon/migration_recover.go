// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// isolationJournalPresent reports whether either journal is on disk, readable
// or not.
func isolationJournalPresent() bool {
	for _, js := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		if _, err := js.Load(); !errors.Is(err, migrate.ErrNoJournal) {
			return true
		}
	}

	return false
}

// recoverIsolationMigration runs at daemon start. A mode change whose journal
// is still on disk was interrupted by a restart or handed off by a reprovision,
// and its outcome was never reported. It takes installation ownership, resolves
// the journal (resuming or rolling back, see resolveJournal), reports the
// outcome in status.isolationChange and removes the journal once the report is
// made. It never fails the daemon: what cannot be done now is tried again at
// the next start.
func recoverIsolationMigration(
	ctx context.Context,
	log *slog.Logger,
	c client.Client,
	store *installstate.Store,
	machineName string,
	operator nodeOperator,
) {
	if !isolationJournalPresent() {
		return
	}

	lock, err := installstate.AcquireWithin(ctx, installationLockWaitTimeout, store.AcquireMutationLock)
	if err != nil {
		log.Warn("could not take installation ownership to resolve an isolation change; trying again at the next start", "error", err)

		return
	}

	defer releaseInstallationLock(log, lock)

	settleIsolationChanges(ctx, log, c, machineName, operator)
}
