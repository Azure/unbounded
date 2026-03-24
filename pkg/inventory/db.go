package inventory

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// DeviceType identifies the class of hardware component.
type DeviceType string

const (
	DeviceTypeChassis DeviceType = "chassis"
	DeviceTypeBMC     DeviceType = "bmc"
	DeviceTypeCPU     DeviceType = "cpu"
	DeviceTypeMemory  DeviceType = "memory"
	DeviceTypeDisk    DeviceType = "disk"
	DeviceTypeNIC     DeviceType = "nic"
	DeviceTypeGPU     DeviceType = "gpu"
)

// DeviceRecord is the central schema for all inventory items. Every collected
// hardware component is normalised into this structure so that heterogeneous
// device data can be stored and queried uniformly.
type DeviceRecord struct {
	DeviceType     DeviceType      `json:"device_type"`
	DeviceName     string          `json:"device_name"`
	HostIdentifier string          `json:"host_identifier"`
	SerialNumber   string          `json:"serial_number"`
	Attributes     json.RawMessage `json:"attributes"`
}

// NeighborRecord maps directly to a row in the neighbors table.
type NeighborRecord struct {
	HostIdentifier string          `json:"host_identifier"`
	LocalInterface string          `json:"local_interface"`
	Attributes     json.RawMessage `json:"attributes"`
}

// mustMarshal serialises v to JSON. Panics on error (attribute structs are
// simple value types so marshalling cannot realistically fail).
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("inventory: failed to marshal attributes: " + err.Error())
	}
	return data
}

// ensureDB opens the database at dbPath (creating the file if needed) and
// ensures all required tables exist.
func ensureDB(dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	const createTable = `
CREATE TABLE IF NOT EXISTS inventory (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    device_type     TEXT    NOT NULL,
    device_name     TEXT    NOT NULL,
    host_identifier TEXT    NOT NULL DEFAULT '',
    serial_number   TEXT    NOT NULL,
    attributes      TEXT    NOT NULL,
    UNIQUE(serial_number)
);`

	const createNeighbors = `
CREATE TABLE IF NOT EXISTS neighbors (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    host_identifier TEXT    NOT NULL,
    local_interface TEXT    NOT NULL,
    attributes      TEXT    NOT NULL,
    UNIQUE(host_identifier, local_interface)
);`

	if _, err := db.Exec(createTable); err != nil {
		return fmt.Errorf("failed to create inventory table: %w", err)
	}

	if _, err := db.Exec(createNeighbors); err != nil {
		return fmt.Errorf("failed to create neighbors table: %w", err)
	}

	return nil
}

// upsertRecords inserts new DeviceRecords or updates existing ones (matched by
// serial_number). A row is only updated when device_name or attributes differ.
func upsertRecords(dbPath string, records []DeviceRecord) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const upsertSQL = `
INSERT INTO inventory (device_type, device_name, host_identifier, serial_number, attributes)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(serial_number) DO UPDATE SET
    device_type     = excluded.device_type,
    device_name     = excluded.device_name,
    host_identifier = excluded.host_identifier,
    attributes      = excluded.attributes
WHERE device_name     != excluded.device_name
   OR host_identifier != excluded.host_identifier
   OR attributes      != excluded.attributes;`

	stmt, err := tx.Prepare(upsertSQL)
	if err != nil {
		return fmt.Errorf("failed to prepare upsert: %w", err)
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(string(r.DeviceType), r.DeviceName, r.HostIdentifier, r.SerialNumber, string(r.Attributes)); err != nil {
			return fmt.Errorf("failed to upsert %s %q: %w", r.DeviceType, r.SerialNumber, err)
		}
	}

	return tx.Commit()
}

// upsertNeighbors inserts new NeighborRecords or updates existing ones
// (matched by host_identifier + local_interface).
func upsertNeighbors(dbPath string, records []NeighborRecord) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const upsertSQL = `
INSERT INTO neighbors (host_identifier, local_interface, attributes)
VALUES (?, ?, ?)
ON CONFLICT(host_identifier, local_interface) DO UPDATE SET
    attributes = excluded.attributes
WHERE attributes != excluded.attributes;`

	stmt, err := tx.Prepare(upsertSQL)
	if err != nil {
		return fmt.Errorf("failed to prepare neighbor upsert: %w", err)
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(r.HostIdentifier, r.LocalInterface, string(r.Attributes)); err != nil {
			return fmt.Errorf("failed to upsert neighbor %q on %q: %w", r.HostIdentifier, r.LocalInterface, err)
		}
	}

	return tx.Commit()
}
