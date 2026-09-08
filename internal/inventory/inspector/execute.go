// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"

	"github.com/Azure/unbounded/internal/inventory/aggregator"
)

// Config holds the configuration for the inventory inspector.
type Config struct {
	Debug  bool
	DbConn pq.Config
}

// conflictRecord represents a single conflict to be written to the conflicts table.
type conflictRecord struct {
	ConflictType string
	Devices      string
}

const createConflictsTableSQL = `CREATE TABLE IF NOT EXISTS conflicts (
	id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	detected_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	conflict_type TEXT NOT NULL,
	devices       TEXT NOT NULL
)`

// Execute connects to the inventory database, runs conflict checks, records
// results, and exits.
func Execute(ctx context.Context, cfg Config) error {
	slog.Info("starting inventory-inspector")

	db, err := aggregator.OpenDatabase(cfg.DbConn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}

	defer db.Close() //nolint:errcheck

	slog.Info("database connection successful",
		"db_host", cfg.DbConn.Host,
		"db_port", cfg.DbConn.Port,
		"db_name", cfg.DbConn.Database,
	)

	if _, err := db.ExecContext(ctx, createConflictsTableSQL); err != nil {
		return fmt.Errorf("creating conflicts table: %w", err)
	}

	var conflicts []conflictRecord

	serialConflicts, err := detectDuplicateSerials(ctx, db)
	if err != nil {
		return fmt.Errorf("detecting duplicate serial numbers: %w", err)
	}

	conflicts = append(conflicts, serialConflicts...)

	lldpConflicts, err := detectDuplicateLLDPUpstreamPorts(ctx, db)
	if err != nil {
		return fmt.Errorf("detecting duplicate LLDP upstream ports: %w", err)
	}

	conflicts = append(conflicts, lldpConflicts...)

	if len(conflicts) == 0 {
		slog.Info("no conflicts detected")
	} else {
		slog.Info("conflicts detected", "count", len(conflicts))

		if err := writeConflicts(ctx, db, conflicts); err != nil {
			return fmt.Errorf("writing conflicts: %w", err)
		}
	}

	slog.Info("inventory-inspector completed")

	return nil
}

// writeConflicts inserts the detected conflicts into the conflicts table.
func writeConflicts(ctx context.Context, db *sql.DB, conflicts []conflictRecord) error {
	now := time.Now()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO conflicts (detected_at, conflict_type, devices) VALUES ($1, $2, $3)`)
	if err != nil {
		return fmt.Errorf("preparing conflict insert: %w", err)
	}

	defer stmt.Close() //nolint:errcheck

	for _, c := range conflicts {
		if _, err := stmt.ExecContext(ctx, now, c.ConflictType, c.Devices); err != nil {
			return fmt.Errorf("inserting conflict: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing conflicts: %w", err)
	}

	slog.Info("conflicts written to database", "count", len(conflicts))

	return nil
}
