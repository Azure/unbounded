// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package aggregator

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	inventoryv1 "github.com/Azure/unbounded/api/inventory/v1"
)

// newTestDB returns a SQLite database holding the inventory and neighbors
// tables. The production DDL is PostgreSQL specific, so the fixture mirrors
// its columns with SQLite types.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing test database: %v", err)
		}
	})

	schema := []string{
		`CREATE TABLE inventory (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			device_type     TEXT NOT NULL,
			device_name     TEXT NOT NULL,
			host_identifier TEXT NOT NULL,
			serial_number   TEXT NOT NULL UNIQUE,
			attributes      TEXT NOT NULL
		)`,
		`CREATE TABLE neighbors (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			host_identifier TEXT NOT NULL,
			local_interface TEXT NOT NULL,
			attributes      TEXT NOT NULL,
			UNIQUE (host_identifier, local_interface)
		)`,
	}

	for _, ddl := range schema {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("creating test schema: %v", err)
		}
	}

	return db
}

func TestUpsertDevicesCountsChangedRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	records := []*inventoryv1.DeviceRecord{
		{
			DeviceType:     "nic",
			DeviceName:     "eth0",
			HostIdentifier: "host-a",
			SerialNumber:   "SN-1",
			Attributes:     []byte(`{"speed":100}`),
		},
	}

	saved, err := UpsertDevices(ctx, db, records)
	if err != nil {
		t.Fatalf("upserting new device: %v", err)
	}

	if saved != 1 {
		t.Fatalf("upserting new device saved = %d, want 1", saved)
	}

	saved, err = UpsertDevices(ctx, db, records)
	if err != nil {
		t.Fatalf("republishing identical inventory: %v", err)
	}

	if saved != 0 {
		t.Errorf("republishing identical inventory saved = %d, want 0", saved)
	}

	records[0].Attributes = []byte(`{"speed":200}`)

	saved, err = UpsertDevices(ctx, db, records)
	if err != nil {
		t.Fatalf("upserting changed attributes: %v", err)
	}

	if saved != 1 {
		t.Errorf("upserting changed attributes saved = %d, want 1", saved)
	}

	records = append(records, &inventoryv1.DeviceRecord{
		DeviceType:     "nic",
		DeviceName:     "eth1",
		HostIdentifier: "host-a",
		SerialNumber:   "SN-2",
		Attributes:     []byte(`{"speed":100}`),
	})

	saved, err = UpsertDevices(ctx, db, records)
	if err != nil {
		t.Fatalf("upserting mixed batch: %v", err)
	}

	if saved != 1 {
		t.Errorf("upserting mixed batch saved = %d, want 1", saved)
	}
}

func TestUpsertNeighborsCountsChangedRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	records := []*inventoryv1.NeighborRecord{
		{
			HostIdentifier: "host-a",
			LocalInterface: "eth0",
			Attributes:     []byte(`{"chassis_id":"aa:bb"}`),
		},
	}

	saved, err := UpsertNeighbors(ctx, db, records)
	if err != nil {
		t.Fatalf("upserting new neighbor: %v", err)
	}

	if saved != 1 {
		t.Fatalf("upserting new neighbor saved = %d, want 1", saved)
	}

	saved, err = UpsertNeighbors(ctx, db, records)
	if err != nil {
		t.Fatalf("republishing identical inventory: %v", err)
	}

	if saved != 0 {
		t.Errorf("republishing identical inventory saved = %d, want 0", saved)
	}

	records[0].Attributes = []byte(`{"chassis_id":"cc:dd"}`)

	saved, err = UpsertNeighbors(ctx, db, records)
	if err != nil {
		t.Fatalf("upserting changed attributes: %v", err)
	}

	if saved != 1 {
		t.Errorf("upserting changed attributes saved = %d, want 1", saved)
	}
}
