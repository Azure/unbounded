// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// startLocalForward listens on 127.0.0.1:localPort on the client host and
// splices each accepted connection to the pod's overlay IP on remotePort,
// dialing it through the userspace overlay stack. It is the inverse of
// startForwarder: it exposes a pod-served TCP service (such as the in-pod
// Redfish server) on a local loopback port. The listener is closed when ctx is
// cancelled.
func startLocalForward(ctx context.Context, o *overlay, localPort, remotePort uint16) error {
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()

		_ = listener.Close() //nolint:errcheck // best-effort close on shutdown
	}()

	go func() {
		for {
			local, err := listener.Accept()
			if err != nil {
				return // listener closed (ctx cancelled).
			}

			go o.serveLocalForward(ctx, local, remotePort)
		}
	}()

	return nil
}

// serveLocalForward dials the pod's overlay IP on remotePort through the overlay
// stack and splices it to an accepted local connection.
func (o *overlay) serveLocalForward(ctx context.Context, local net.Conn, remotePort uint16) {
	remote := tcpip.FullAddress{
		NIC:  overlayNICID,
		Addr: tcpip.AddrFromSlice(o.remoteIP),
		Port: remotePort,
	}

	conn, err := gonet.DialContextTCP(ctx, o.stack, remote, ipv4.ProtocolNumber)
	if err != nil {
		fmt.Printf("  local forward -> %s:%d: dial error: %v\n", o.remoteIP, remotePort, err)

		_ = local.Close() //nolint:errcheck // best-effort close

		return
	}

	proxyConn(ctx, local, conn)
}
