// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// tftpPort is the well-known TFTP server port. PXE clients on the overlay
	// send their read requests to <overlay-ip>:69.
	tftpPort = 69
	// tftpReadBuffer bounds a single relayed datagram. It must be large enough
	// to hold a DATA packet using the largest block size the guest may
	// negotiate (before we clamp it) plus the 4-byte TFTP header.
	tftpReadBuffer = 4096
	// tftpIdleTimeout tears down a session that stalls in either direction.
	tftpIdleTimeout = 15 * time.Second
	// tftpOverlayOverhead is the per-DATA-packet overhead that must be
	// subtracted from the overlay MTU when computing the largest safe TFTP
	// block size: 20 (IPv4 header) + 8 (UDP header) + 4 (TFTP DATA header).
	tftpOverlayOverhead = 32
	// tftpMinBlkSize is the TFTP default/minimum block size (RFC 1350). We
	// never clamp below this.
	tftpMinBlkSize = 512
)

// startTFTPProxy installs a userspace UDP forwarder on the overlay stack that
// relays TFTP (PXE) traffic addressed to the client's overlay IP:69 to an
// upstream TFTP server (typically the client-side dnsmasq). This lets a
// diskless guest on the pod side network-boot off the client. Datagrams to any
// other port are dropped. The proxy runs until ctx is cancelled.
func (o *overlay) startTFTPProxy(ctx context.Context, upstream *net.UDPAddr) {
	handler := func(req *udp.ForwarderRequest) {
		if req.ID().LocalPort != tftpPort {
			return // only serve the TFTP port; drop everything else.
		}

		var wq waiter.Queue

		ep, tcpErr := req.CreateEndpoint(&wq)
		if tcpErr != nil {
			fmt.Printf("  tftp: create endpoint: %s\n", tcpErr)
			return
		}

		guest := gonet.NewUDPConn(o.stack, &wq, ep)

		go o.serveTFTP(ctx, guest, upstream)
	}

	fwd := udp.NewForwarder(o.stack, handler)
	o.stack.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)
}

// serveTFTP proxies a single TFTP session. The guest talks to the overlay
// endpoint (always <overlay-ip>:69 from its perspective), and a single
// unconnected host socket carries traffic to the upstream server. TFTP servers
// answer the initial request from a fresh transfer ID (TID) port, so the host
// side starts by sending to <upstream>:69 and then latches onto whatever port
// the server replies from for the remainder of the transfer.
func (o *overlay) serveTFTP(ctx context.Context, guest *gonet.UDPConn, upstream *net.UDPAddr) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var laddr *net.UDPAddr
	if o.proxySrc != nil {
		// Bind the egress source to the guest's real overlay IP so metalman
		// (bound on the host loopback) observes that IP as the request source.
		laddr = &net.UDPAddr{IP: o.proxySrc}
	}

	host, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		fmt.Printf("  tftp: open host socket: %v\n", err)

		_ = guest.Close() //nolint:errcheck // best-effort close

		return
	}

	go func() {
		<-ctx.Done()

		_ = guest.Close() //nolint:errcheck // best-effort close
		_ = host.Close()  //nolint:errcheck // best-effort close
	}()

	var (
		mu     sync.Mutex
		server = &net.UDPAddr{IP: upstream.IP, Port: upstream.Port}
	)

	// blkCap is the largest TFTP block size whose DATA packets still fit within
	// the overlay MTU. The guest firmware/shim/grub may request a much larger
	// blksize (for example 1468) which metalman's TFTP server honors verbatim;
	// left unclamped those DATA packets exceed the overlay MTU and are dropped,
	// silently corrupting large transfers (for example grubx64.efi). Clamping
	// the request here makes the server negotiate a fitting block size.
	blkCap := o.mtu - tftpOverlayOverhead

	var wg sync.WaitGroup

	wg.Add(2)

	// guest -> server: forward guest datagrams to the current server address.
	go func() {
		defer wg.Done()
		defer cancel()

		buf := make([]byte, tftpReadBuffer)

		for {
			_ = guest.SetReadDeadline(time.Now().Add(tftpIdleTimeout)) //nolint:errcheck // deadline is best-effort

			n, readErr := guest.Read(buf)
			if n > 0 {
				out := clampTFTPBlksize(buf[:n], blkCap)

				mu.Lock()
				dst := *server
				mu.Unlock()

				if _, writeErr := host.WriteToUDP(out, &dst); writeErr != nil {
					return
				}
			}

			if readErr != nil {
				return
			}
		}
	}()

	// server -> guest: forward server replies, latching onto the server TID.
	go func() {
		defer wg.Done()
		defer cancel()

		buf := make([]byte, tftpReadBuffer)

		for {
			_ = host.SetReadDeadline(time.Now().Add(tftpIdleTimeout)) //nolint:errcheck // deadline is best-effort

			n, from, readErr := host.ReadFromUDP(buf)
			if n > 0 {
				if from != nil {
					mu.Lock()
					server = from
					mu.Unlock()
				}

				if _, writeErr := guest.Write(buf[:n]); writeErr != nil {
					return
				}
			}

			if readErr != nil {
				return
			}
		}
	}()

	wg.Wait()
}

// clampTFTPBlksize rewrites a TFTP read request (RRQ) so its negotiated
// "blksize" option does not exceed cap, returning the packet to forward. Any
// non-RRQ packet, or an RRQ that requests no blksize (or one already <= cap),
// is returned unchanged. This keeps DATA packets within the overlay MTU: the
// upstream server (metalman's pin/tftp) honors the client's requested blksize
// verbatim, so we lower the request rather than the reply.
//
// RRQ wire format (RFC 1350/2347): 2-byte opcode 0x0001, then a sequence of
// NUL-terminated strings: filename, mode, then zero or more option/value pairs.
func clampTFTPBlksize(pkt []byte, cap int) []byte {
	const opRRQ = 1

	if cap < tftpMinBlkSize {
		cap = tftpMinBlkSize
	}

	if len(pkt) < 2 || int(pkt[0])<<8|int(pkt[1]) != opRRQ {
		return pkt
	}

	// Split the fields after the opcode into NUL-terminated tokens.
	fields := splitNulFields(pkt[2:])
	if len(fields) < 2 {
		return pkt
	}

	// Options start after filename (index 0) and mode (index 1) and come in
	// (name, value) pairs.
	changed := false

	for i := 2; i+1 < len(fields); i += 2 {
		if !strings.EqualFold(string(fields[i]), "blksize") {
			continue
		}

		requested, err := strconv.Atoi(string(fields[i+1]))
		if err != nil {
			return pkt
		}

		if requested > cap {
			fields[i+1] = []byte(strconv.Itoa(cap))
			changed = true
		}

		break
	}

	if !changed {
		return pkt
	}

	// Rebuild the packet: opcode + NUL-terminated fields.
	out := make([]byte, 0, len(pkt)+4)
	out = append(out, pkt[0], pkt[1])

	for _, f := range fields {
		out = append(out, f...)
		out = append(out, 0)
	}

	return out
}

// splitNulFields splits b into the NUL-terminated tokens it contains. A
// trailing unterminated remainder is ignored (a well-formed RRQ ends every
// field with NUL).
func splitNulFields(b []byte) [][]byte {
	var fields [][]byte

	start := 0

	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			fields = append(fields, b[start:i])
			start = i + 1
		}
	}

	return fields
}
