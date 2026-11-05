// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

// reportStartupIsolationChanges reports, in status.isolationChange, the mode
// changes discoverAndMigrate resolved on the host before the daemon had a
// client, and removes each journal once its terminal outcome is reported. It
// takes installation ownership again for that, since it removes journals. It
// never fails the daemon: what cannot be reported now is reported at the next
// repave or start, from the journal that stays.
func reportStartupIsolationChanges(
	ctx context.Context,
	log *slog.Logger,
	c client.Client,
	opts *runOptions,
	machineName string,
	settled []settledJournal,
) settleOutcome {
	if len(settled) == 0 {
		return settleOutcome{}
	}

	lock, err := installstate.AcquireWithin(ctx, installationLockWaitTimeout, opts.installation.AcquireMutationLock)
	if err != nil {
		log.Warn("could not take installation ownership to report an isolation change; it is reported at the next repave or start", "error", err)

		// A reprovision that is still running is still running.
		var res settleOutcome

		for _, s := range settled {
			if s.unreadable == nil && errors.Is(s.outcome, ErrMigrationHandedOff) {
				res.handedOff = true
			}
		}

		return res
	}

	defer releaseInstallationLock(log, lock)

	return opts.Migration.reportSettled(ctx, log, c, machineName, settled)
}
