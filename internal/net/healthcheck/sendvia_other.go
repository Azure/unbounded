// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package healthcheck

import "net"

// writeVia sends data to addr. Choosing the interface per packet is Linux
// only; elsewhere the routing table picks it.
func writeVia(conn net.PacketConn, data []byte, addr *net.UDPAddr, _ int) error {
	_, err := conn.WriteTo(data, addr)
	return err
}
