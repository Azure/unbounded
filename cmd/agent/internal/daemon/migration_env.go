// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// DefaultMigrationReadyTimeout bounds the wait for the target node stack to come
// up on the host.
const DefaultMigrationReadyTimeout = 5 * time.Minute

// ErrMigrationHandedOff is returned by MigrateIsolation when the change
// continues without this daemon: a reprovision resets the host, which removes
// the daemon, and a handoff command finishes the bootstrap. The change stays
// InProgress in the Machine status and the daemon that starts afterward
// reports it.
var ErrMigrationHandedOff = errors.New("isolation migration handed off to the reprovision unit")

const (
	// migrationJournalName is the in-place journal, next to the applied config.
	migrationJournalName = appliedconfig.InPlaceJournalName

	// ReprovisionDir holds what a reprovision needs to survive its own reset:
	// the journal, a copy of the agent binary and the target configuration. The
	// reset does not remove it; the daemon that reports the outcome does.
	ReprovisionDir = "/var/lib/unbounded-agent-reprovision"
	// ReprovisionJournalName, ReprovisionConfigName and ReprovisionBinaryName
	// are the files in ReprovisionDir.
	ReprovisionJournalName = "journal.json"
	ReprovisionConfigName  = "config.json"
	ReprovisionBinaryName  = "unbounded-agent"
	// ReprovisionUnit is the systemd unit that runs the handoff command.
	ReprovisionUnit = "unbounded-agent-reprovision.service"
	// ReprovisionMaxAttempts is how many runs of the handoff command may start
	// before it gives up. It stays below the unit's StartLimitBurst, so the
	// handoff records why it stopped before systemd stops it without a word.
	ReprovisionMaxAttempts = 5
)

// Paths are variables so tests can point them at a temporary tree.
var (
	inPlaceJournalPath    = func() string { return filepath.Join(goalstates.AgentConfigDir, migrationJournalName) }
	reprovisionDir        = func() string { return ReprovisionDir }
	reprovisionUnitDir    = func() string { return goalstates.SystemdSystemDir }
	reprovisionJournalOf  = func() string { return filepath.Join(reprovisionDir(), ReprovisionJournalName) }
	inPlaceJournalStore   = func() migrate.JournalStore { return migrate.JournalStore{Path: inPlaceJournalPath()} }
	reprovisionJournalFor = func() migrate.JournalStore { return migrate.JournalStore{Path: reprovisionJournalOf()} }

	// inPlaceConfigSnapshotDir holds the saved shared configuration, next to the
	// journal, so a restarted daemon can still restore it.
	inPlaceConfigSnapshotDir = func() string {
		return filepath.Join(filepath.Dir(inPlaceJournalPath()), inPlaceConfigSnapshotName)
	}
	// migrationConfigRoot prefixes the shared configuration paths. Empty is the
	// real host.
	migrationConfigRoot  = func() string { return "" }
	migrationConfigPaths = confsnap.SharedPaths
)

// inPlaceConfigSnapshotName is the directory of the saved shared configuration.
const inPlaceConfigSnapshotName = "isolation-migration-config"
