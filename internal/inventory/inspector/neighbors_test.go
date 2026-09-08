// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// marshalAttrs returns the JSON encoding of attrs as a string.
func marshalAttrs(attrs lldpAttributes) string {
	raw, err := json.Marshal(attrs)
	if err != nil {
		panic(err)
	}

	return string(raw)
}

func TestDetectDuplicateLLDP_SamePeerSamePort(t *testing.T) {
	// Two hosts report the same upstream chassis + port - this is a conflict.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"}).
		AddRow("host-a", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"})).
		AddRow("host-b", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"}))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	c := conflicts[0]
	if c.ConflictType != "duplicate_lldp_upstream_port" {
		t.Errorf("expected conflict type %q, got %q", "duplicate_lldp_upstream_port", c.ConflictType)
	}

	if !strings.Contains(c.Devices, "chassis=switch-1") {
		t.Errorf("expected Devices to mention chassis, got %q", c.Devices)
	}

	if !strings.Contains(c.Devices, "port=Gi0/1") {
		t.Errorf("expected Devices to mention port, got %q", c.Devices)
	}

	if !strings.Contains(c.Devices, "host-a") || !strings.Contains(c.Devices, "host-b") {
		t.Errorf("expected Devices to mention both hosts, got %q", c.Devices)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateLLDP_DifferentPortsSamePeer(t *testing.T) {
	// Two hosts connected to different ports on the same switch - no conflict.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"}).
		AddRow("host-a", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"})).
		AddRow("host-b", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/2"}))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts for different ports on the same peer, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateLLDP_SamePortDifferentPeers(t *testing.T) {
	// Two hosts connected to the same port identifier but on different switches - no conflict.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"}).
		AddRow("host-a", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"})).
		AddRow("host-b", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-2", PortID: "Gi0/1"}))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts for same port on different peers, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateLLDP_MultipleConflicts(t *testing.T) {
	// Two separate upstream ports each claimed by two hosts.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"}).
		AddRow("host-a", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"})).
		AddRow("host-b", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: "Gi0/1"})).
		AddRow("host-c", "eth1", marshalAttrs(lldpAttributes{ChassisID: "switch-2", PortID: "Gi0/5"})).
		AddRow("host-d", "eth1", marshalAttrs(lldpAttributes{ChassisID: "switch-2", PortID: "Gi0/5"}))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateLLDP_SkipsMissingChassisOrPort(t *testing.T) {
	// Rows with empty chassisID or portID should be silently skipped.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"}).
		AddRow("host-a", "eth0", marshalAttrs(lldpAttributes{ChassisID: "", PortID: "Gi0/1"})).
		AddRow("host-b", "eth0", marshalAttrs(lldpAttributes{ChassisID: "switch-1", PortID: ""})).
		AddRow("host-c", "eth0", marshalAttrs(lldpAttributes{ChassisID: "", PortID: ""}))

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts when chassis/port are empty, got %d", len(conflicts))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDetectDuplicateLLDP_NoNeighbors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("creating mock: %v", err)
	}
	defer db.Close() //nolint:errcheck

	rows := sqlmock.NewRows([]string{"host_identifier", "local_interface", "attributes"})

	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	conflicts, err := detectDuplicateLLDPUpstreamPorts(context.Background(), db)
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
