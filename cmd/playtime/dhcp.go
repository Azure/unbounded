// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	// dhcpServerPort is the well-known BOOTPS/DHCP server port.
	dhcpServerPort = 67
	// dhcpClientPort is the well-known BOOTPC/DHCP client port.
	dhcpClientPort = 68
	// dhcpReplyReadBuffer bounds a single relayed reply. DHCP messages are
	// small; 4 KiB comfortably covers the fixed header plus options.
	dhcpReplyReadBuffer = 4096
)

// broadcastMAC is used as the Ethernet destination for a DHCP reply when the
// relay has not learned the client's MAC (should not normally happen).
var broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// dhcpClient records what the relay learned about a pending DHCP transaction so
// the matching reply can be steered correctly.
type dhcpClient struct {
	mac        net.HardwareAddr
	httpClient bool
}

// dhcpRelay bridges DHCP between the overlay (pod side) and an upstream DHCP
// server reachable from the client host. Requests are intercepted off the
// overlay packet pump, stamped with giaddr, and unicast to the server from a
// host UDP socket; the server's replies (unicast back to giaddr) are wrapped in
// fresh Ethernet/IP/UDP headers and pushed back onto the overlay toward the pod.
type dhcpRelay struct {
	serverAddr *net.UDPAddr
	giaddr     net.IP
	overlayIP  net.IP
	overlayMAC net.HardwareAddr
	conn       *net.UDPConn
	sendToPod  func([]byte)
	boot       *bootState

	mu      sync.Mutex
	clients map[dhcpv4.TransactionID]dhcpClient
}

// newDHCPRelay opens the host relay socket and resolves the giaddr used to steer
// server replies back to this host. overlayMAC is the client's overlay link
// address (Ethernet source of relayed replies), sendToPod delivers a raw inner
// Ethernet frame onto the overlay toward the pod, and boot carries the current
// UEFI HTTP boot intent (may be nil) used to steer HTTP-boot clients.
func newDHCPRelay(cfg Config, overlayMAC net.HardwareAddr, sendToPod func([]byte), boot *bootState) (*dhcpRelay, error) {
	serverAddr, err := cfg.dhcpServerAddr()
	if err != nil {
		return nil, err
	}

	overlayIP := net.ParseIP(cfg.OverlayLocalIP).To4()
	if overlayIP == nil {
		return nil, fmt.Errorf("invalid overlay local ip %q", cfg.OverlayLocalIP)
	}

	giaddr, err := cfg.dhcpGiaddr()
	if err != nil {
		return nil, err
	}

	if giaddr == nil {
		giaddr, err = detectSourceIP(serverAddr)
		if err != nil {
			return nil, fmt.Errorf("auto-detect giaddr (set --dhcp-giaddr): %w", err)
		}
	}

	conn, err := listenRelaySocket(giaddr, cfg.DHCPRelayPort)
	if err != nil {
		return nil, fmt.Errorf("bind dhcp relay %s:%d (port 67 needs root or CAP_NET_BIND_SERVICE): %w", giaddr, cfg.DHCPRelayPort, err)
	}

	return &dhcpRelay{
		serverAddr: serverAddr,
		giaddr:     giaddr,
		overlayIP:  overlayIP,
		overlayMAC: overlayMAC,
		conn:       conn,
		sendToPod:  sendToPod,
		boot:       boot,
		clients:    make(map[dhcpv4.TransactionID]dhcpClient),
	}, nil
}

// listenRelaySocket binds the relay's UDP socket to giaddr:port. Per RFC 2131 a
// DHCP server unicasts its replies to giaddr on the server port, so the relay
// must listen on that specific address. SO_REUSEADDR lets the relay coexist
// with another DHCP server already bound to the wildcard address on the same
// host (for example a libvirt dnsmasq on 0.0.0.0:67).
func listenRelaySocket(giaddr net.IP, port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var opErr error

			if err := c.Control(func(fd uintptr) {
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}

			return opErr
		},
	}

	addr := net.JoinHostPort(giaddr.String(), strconv.Itoa(port))

	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return nil, err
	}

	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close() //nolint:errcheck // best-effort close on an unexpected type
		return nil, fmt.Errorf("relay socket is not a *net.UDPConn (got %T)", pc)
	}

	return conn, nil
}

// detectSourceIP returns the local IPv4 address the host kernel would use to
// reach the DHCP server. It is used as giaddr so the server can route its
// replies back to this host. No packet is sent: connecting a UDP socket only
// performs a route lookup.
func detectSourceIP(server *net.UDPAddr) (net.IP, error) {
	c, err := net.DialUDP("udp4", nil, server)
	if err != nil {
		return nil, err
	}
	defer c.Close() //nolint:errcheck // best-effort close of a route-probe socket

	local, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil {
		return nil, fmt.Errorf("could not determine local address toward %s", server)
	}

	ip := local.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("local address toward %s is not IPv4", server)
	}

	return ip, nil
}

// handleFromPod inspects an inner Ethernet frame from the pod. If it carries a
// DHCP BOOTREQUEST it is stamped and forwarded to the upstream server and true
// is returned so the caller skips normal overlay delivery. Any other frame
// returns false and is delivered normally.
func (r *dhcpRelay) handleFromPod(frame []byte) bool {
	msg, clientMAC, ok := parseDHCPRequest(frame)
	if !ok {
		return false
	}

	r.mu.Lock()
	r.clients[msg.TransactionID] = dhcpClient{
		mac:        clientMAC,
		httpClient: strings.Contains(msg.ClassIdentifier(), "HTTPClient"),
	}
	r.mu.Unlock()

	stampRelay(msg, r.giaddr)

	if _, err := r.conn.WriteToUDP(msg.ToBytes(), r.serverAddr); err != nil {
		fmt.Printf("dhcp relay: forward to %s failed: %v\n", r.serverAddr, err)
	}

	return true
}

// run reads replies from the upstream DHCP server and relays them onto the
// overlay toward the pod until the context is cancelled.
func (r *dhcpRelay) run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		r.conn.Close() //nolint:errcheck // best-effort close to unblock ReadFromUDP
	}()

	buf := make([]byte, dhcpReplyReadBuffer)

	for {
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}

			continue
		}

		msg, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || msg.OpCode != dhcpv4.OpcodeBootReply {
			continue
		}

		r.deliverToPod(msg)
	}
}

// deliverToPod wraps a DHCP reply in Ethernet/IP/UDP headers and pushes it onto
// the overlay toward the client whose request carried the same transaction ID.
func (r *dhcpRelay) deliverToPod(msg *dhcpv4.DHCPv4) {
	r.mu.Lock()

	client, ok := r.clients[msg.TransactionID]
	if ok {
		delete(r.clients, msg.TransactionID)
	}
	r.mu.Unlock()

	dstMAC := broadcastMAC
	if ok {
		dstMAC = client.mac
	}

	if ok && client.httpClient {
		r.steerHTTPBoot(msg)
	}

	frame := buildReplyFrame(msg.ToBytes(), dstMAC, r.overlayMAC, r.overlayIP)
	r.sendToPod(frame)
}

// steerHTTPBoot rewrites a DHCP reply destined for a UEFI HTTP-boot client so
// the guest fetches its boot image from the client-side HTTP boot proxy. It only
// acts when Redfish currently requests HTTP boot; otherwise the reply is left
// untouched (for example so a plain PXE attempt still works). The upstream lease
// (address, subnet, router) is preserved so the guest stays overlay-routable.
func (r *dhcpRelay) steerHTTPBoot(msg *dhcpv4.DHCPv4) {
	if r.boot == nil {
		return
	}

	httpBoot, bootURI := r.boot.get()
	if !httpBoot || bootURI == "" {
		return
	}

	target, err := url.Parse(bootURI)
	if err != nil {
		return
	}

	rewritten := fmt.Sprintf("http://%s:%d%s", r.overlayIP, httpBootProxyPort, target.RequestURI())

	msg.UpdateOption(dhcpv4.OptClassIdentifier("HTTPClient"))
	msg.UpdateOption(dhcpv4.OptBootFileName(rewritten))
}

// close releases the relay socket.
func (r *dhcpRelay) close() {
	r.conn.Close() //nolint:errcheck // best-effort close on teardown
}

// stampRelay marks a DHCP request as relayed: it sets giaddr (so the server
// unicasts its reply back to the relay) when unset and increments the hop count.
func stampRelay(msg *dhcpv4.DHCPv4, giaddr net.IP) {
	if msg.GatewayIPAddr == nil || msg.GatewayIPAddr.IsUnspecified() {
		msg.GatewayIPAddr = giaddr
	}

	msg.HopCount++
}

// parseDHCPRequest parses an inner Ethernet frame and, if it carries a DHCP
// BOOTREQUEST (IPv4/UDP to the server port), returns the decoded message and the
// client's Ethernet source address.
func parseDHCPRequest(frame []byte) (*dhcpv4.DHCPv4, net.HardwareAddr, bool) {
	if len(frame) < header.EthernetMinimumSize {
		return nil, nil, false
	}

	eth := header.Ethernet(frame)
	if eth.Type() != header.IPv4ProtocolNumber {
		return nil, nil, false
	}

	ipBytes := frame[header.EthernetMinimumSize:]
	if len(ipBytes) < header.IPv4MinimumSize {
		return nil, nil, false
	}

	ip := header.IPv4(ipBytes)
	if !ip.IsValid(len(ipBytes)) || ip.Protocol() != uint8(header.UDPProtocolNumber) {
		return nil, nil, false
	}

	udpBytes := ip.Payload()
	if len(udpBytes) < header.UDPMinimumSize {
		return nil, nil, false
	}

	udp := header.UDP(udpBytes)
	if udp.DestinationPort() != dhcpServerPort {
		return nil, nil, false
	}

	msg, err := dhcpv4.FromBytes(udp.Payload())
	if err != nil || msg.OpCode != dhcpv4.OpcodeBootRequest {
		return nil, nil, false
	}

	srcMAC := net.HardwareAddr(append([]byte(nil), []byte(eth.SourceAddress())...))

	return msg, srcMAC, true
}

// buildReplyFrame wraps a DHCP reply payload in UDP/IPv4/Ethernet headers
// destined for the overlay client. The IP destination is the limited broadcast
// address so the client accepts it before its lease is configured, while the
// Ethernet destination is unicast to the learned client MAC when known.
func buildReplyFrame(payload []byte, dstMAC, srcMAC net.HardwareAddr, srcIP net.IP) []byte {
	const (
		ethLen = header.EthernetMinimumSize
		ipLen  = header.IPv4MinimumSize
		udpLen = header.UDPMinimumSize
	)

	frame := make([]byte, ethLen+ipLen+udpLen+len(payload))

	srcAddr := tcpip.AddrFrom4Slice(srcIP.To4())
	dstAddr := tcpip.AddrFrom4Slice([]byte{255, 255, 255, 255})

	eth := header.Ethernet(frame[:ethLen])
	eth.Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(srcMAC),
		DstAddr: tcpip.LinkAddress(dstMAC),
		Type:    header.IPv4ProtocolNumber,
	})

	udpTotal := uint16(udpLen + len(payload))
	udp := header.UDP(frame[ethLen+ipLen:])
	udp.Encode(&header.UDPFields{
		SrcPort: dhcpServerPort,
		DstPort: dhcpClientPort,
		Length:  udpTotal,
	})
	copy(frame[ethLen+ipLen+udpLen:], payload)

	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, srcAddr, dstAddr, udpTotal)
	xsum = checksum.Checksum(payload, xsum)
	udp.SetChecksum(^udp.CalculateChecksum(xsum))

	ip := header.IPv4(frame[ethLen : ethLen+ipLen])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen) + udpTotal,
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     srcAddr,
		DstAddr:     dstAddr,
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	return frame
}
