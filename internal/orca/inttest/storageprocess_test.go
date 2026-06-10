// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest && storageboundary

package inttest

import (
	"fmt"
	"net"
	"testing"
)

func TestReserveLoopbackPortsHoldsUniquePorts(t *testing.T) {
	t.Parallel()

	reservations := reserveLoopbackPorts(t, 4)
	defer closeLoopbackPortReservations(reservations)

	seen := make(map[int]struct{}, len(reservations))
	for _, reservation := range reservations {
		if _, ok := seen[reservation.port]; ok {
			t.Fatalf("duplicate reserved port %d", reservation.port)
		}

		seen[reservation.port] = struct{}{}

		addr := fmt.Sprintf("127.0.0.1:%d", reservation.port)

		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close() //nolint:errcheck // best-effort

			t.Fatalf("reserved port %s was bindable", addr)
		}
	}
}
