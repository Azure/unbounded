// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// writeVia sends data to addr out of the interface with index ifIndex. The
// interface is set per packet with IP_PKTINFO/IPV6_PKTINFO, so the kernel uses
// it even when the routing table would pick another one, while replies still
// arrive on the shared socket.
func writeVia(conn net.PacketConn, data []byte, addr *net.UDPAddr, ifIndex int) error {
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		return fmt.Errorf("connection is %T, not *net.UDPConn", conn)
	}

	var oob []byte
	if addr.IP.To4() != nil {
		oob = unix.PktInfo4(&unix.Inet4Pktinfo{Ifindex: int32(ifIndex)}) //nolint:gosec // kernel interface indexes fit in int32
	} else {
		oob = unix.PktInfo6(&unix.Inet6Pktinfo{Ifindex: uint32(ifIndex)}) //nolint:gosec // kernel interface indexes are positive
	}

	_, _, err := udp.WriteMsgUDP(data, oob, addr)

	return err
}
