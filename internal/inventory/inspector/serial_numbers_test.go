// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inspector

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDetectDuplicateSerials_SameTypeDifferentHosts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"device_type", "serial_number", "hosts"}).
		AddRow("gpu", "SN-001", []byte("{host-a,host-b}"))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateSerials(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	c := conflicts[0]
	if c.ConflictType != "duplicate_serial_number" {
		t.Errorf("expected conflict type %q, got %q", "duplicate_serial_number", c.ConflictType)
	}

	if !strings.Contains(c.Devices, "serial=SN-001") {
		t.Errorf("expected Devices to mention serial SN-001, got %q", c.Devices)
	}

	if !strings.Contains(c.Devices, "host-a") || !strings.Contains(c.Devices, "host-b") {
		t.Errorf("expected Devices to mention both hosts, got %q", c.Devices)
	}

	if !strings.Contains(c.Devices, "type=gpu") {
		t.Errorf("expected Devices to mention device type, got %q", c.Devices)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateSerials_MultipleConflicts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"device_type", "serial_number", "hosts"}).
		AddRow("gpu", "SN-001", []byte("{host-a,host-b}")).
		AddRow("gpu", "SN-002", []byte("{host-c,host-d,host-e}"))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateSerials(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	if !strings.Contains(conflicts[1].Devices, "SN-002") {
		t.Errorf("expected second conflict for SN-002, got %q", conflicts[1].Devices)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateSerials_DifferentTypeSameSerial(t *testing.T) {
	// When a GPU on host-a and a NIC on host-b share the same serial number,
	// the inspector must not flag a conflict because serial numbers are only
	// meaningful within a single device type.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	// The query groups by (device_type, serial_number), so two different
	// device types each appearing on exactly one host produce no rows.
	rows := sqlmock.NewRows([]string{"device_type", "serial_number", "hosts"})

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateSerials(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts for different device types sharing a serial, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateSerials_NoDuplicates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"device_type", "serial_number", "hosts"})

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateSerials(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}
