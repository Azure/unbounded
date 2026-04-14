// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventorycollector

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	inventoryv1 "github.com/Azure/unbounded-kube/api/inventory/v1"
)

// createTableSQL maps each required table name to the DDL statement that
// creates it.  The statements are written to be idempotent (IF NOT EXISTS).
var createTableSQL = map[string]string{
	"inventory": `CREATE TABLE IF NOT EXISTS inventory (
		id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		device_type     TEXT NOT NULL,
		device_name     TEXT NOT NULL,
		host_identifier TEXT NOT NULL,
		serial_number   TEXT NOT NULL UNIQUE,
		attributes      TEXT NOT NULL
	)`,
	"neighbors": `CREATE TABLE IF NOT EXISTS neighbors (
		id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		host_identifier TEXT NOT NULL,
		local_interface TEXT NOT NULL,
		attributes      TEXT NOT NULL,
		UNIQUE (host_identifier, local_interface)
	)`,
}

// OpenDatabase creates and verifies a connection to the PostgreSQL database
// using the provided pq.Config.
func OpenDatabase(cfg pq.Config) (*sql.DB, error) {
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating database connector: %w", err)
	}

	db := sql.OpenDB(connector)

	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck // best-effort close on failed ping

		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	return db, nil
}

// EnsureSchema checks that the connection targets the expected database and
// creates any missing tables.
func EnsureSchema(db *sql.DB, expectedDB string) error {
	var dbName string
	if err := db.QueryRow("SELECT current_database()").Scan(&dbName); err != nil {
		return fmt.Errorf("querying current database name: %w", err)
	}

	if dbName != expectedDB {
		return fmt.Errorf("connected to database %q, expected %q", dbName, expectedDB)
	}

	for table, ddl := range createTableSQL {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("creating table %q: %w", table, err)
		}
	}

	return nil
}

// UpsertDevices inserts or updates device records into the PostgreSQL
// inventory table. Conflicts on serial_number trigger an update when any
// mutable column differs.
func UpsertDevices(ctx context.Context, db *sql.DB, records []*inventoryv1.DeviceRecord) (int, error) {
	const upsertSQL = `
INSERT INTO inventory (device_type, device_name, host_identifier, serial_number, attributes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(serial_number) DO UPDATE SET
    device_type     = EXCLUDED.device_type,
    device_name     = EXCLUDED.device_name,
    host_identifier = EXCLUDED.host_identifier,
    attributes      = EXCLUDED.attributes
WHERE inventory.device_name     != EXCLUDED.device_name
   OR inventory.host_identifier != EXCLUDED.host_identifier
   OR inventory.attributes      != EXCLUDED.attributes;`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return 0, fmt.Errorf("preparing device upsert: %w", err)
	}

	defer stmt.Close() //nolint:errcheck

	saved := 0

	for _, r := range records {
		if _, err := stmt.ExecContext(ctx, r.DeviceType, r.DeviceName, r.HostIdentifier, r.SerialNumber, string(r.Attributes)); err != nil {
			return saved, fmt.Errorf("upserting device %q: %w", r.SerialNumber, err)
		}

		saved++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing device upsert: %w", err)
	}

	return saved, nil
}

// UpsertNeighbors inserts or updates neighbor records into the PostgreSQL
// neighbors table. Conflicts on (host_identifier, local_interface) trigger an
// update when attributes differ.
func UpsertNeighbors(ctx context.Context, db *sql.DB, records []*inventoryv1.NeighborRecord) (int, error) {
	const upsertSQL = `
INSERT INTO neighbors (host_identifier, local_interface, attributes)
VALUES ($1, $2, $3)
ON CONFLICT(host_identifier, local_interface) DO UPDATE SET
    attributes = EXCLUDED.attributes
WHERE neighbors.attributes != EXCLUDED.attributes;`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return 0, fmt.Errorf("preparing neighbor upsert: %w", err)
	}

	defer stmt.Close() //nolint:errcheck

	saved := 0

	for _, r := range records {
		if _, err := stmt.ExecContext(ctx, r.HostIdentifier, r.LocalInterface, string(r.Attributes)); err != nil {
			return saved, fmt.Errorf("upserting neighbor %q on %q: %w", r.HostIdentifier, r.LocalInterface, err)
		}

		saved++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing neighbor upsert: %w", err)
	}

	return saved, nil
}
