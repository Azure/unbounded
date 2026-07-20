// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	xicmp "golang.org/x/net/icmp"
	xipv4 "golang.org/x/net/ipv4"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	gicmp "gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// wgTunnelMTU is the MTU assigned to each userspace WireGuard netstack device.
// It must leave room under the host path MTU for the WireGuard overhead (up to
// 80 bytes) while comfortably carrying a fully-encapsulated overlay frame
// (overlay MTU + 14 byte inner Ethernet header + 8 byte VXLAN header).
const wgTunnelMTU = 1420

// overlayNICID is the NIC id of the single interface on the overlay stack.
const overlayNICID = tcpip.NICID(1)

// vxlanHeaderSize is the length of the VXLAN header prepended to each inner
// Ethernet frame.
const vxlanHeaderSize = 8

// wgKeyBase64ToHex converts a standard-base64 WireGuard key (as stored in the
// config and key files) into the hex encoding the wireguard-go UAPI expects.
func wgKeyBase64ToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("decode wireguard key: %w", err)
	}

	if len(raw) != 32 {
		return "", fmt.Errorf("wireguard key must be 32 bytes, got %d", len(raw))
	}

	return hex.EncodeToString(raw), nil
}

// vxlanHeader builds the 8-byte VXLAN header for the given VNI with the I
// (instance/VNI valid) flag set.
func vxlanHeader(vni int) []byte {
	h := make([]byte, vxlanHeaderSize)
	h[0] = 0x08 // I flag: VNI is valid.
	h[4] = byte(vni >> 16)
	h[5] = byte(vni >> 8)
	h[6] = byte(vni)

	return h
}

// underlay is the userspace WireGuard transport: one wireguard-go device (each
// with its own gVisor netstack) per gateway. Only device 0 carries outbound
// VXLAN traffic; inbound VXLAN is read from every device so return traffic
// hashed to any gateway by the remote dataplane is received.
type underlay struct {
	gateways []gateway
	devices  []*device.Device
	conns    []*gonet.UDPConn
}

// newUnderlay brings up one userspace WireGuard device per gateway and opens a
// VXLAN UDP listener on each device's netstack. podIP is the demo pod's underlay
// address; it is added to every peer's allowed_ips so cryptokey routing carries
// the VXLAN traffic (see buildUAPI).
func newUnderlay(cfg Config, privHex, podIP string) (*underlay, error) {
	gws := cfg.gateways()
	if len(gws) == 0 {
		return nil, fmt.Errorf("no gateways configured")
	}

	wgAddr, err := cfg.wgAddress()
	if err != nil {
		return nil, err
	}

	localAddr, err := netip.ParsePrefix(wgAddr)
	if err != nil {
		return nil, fmt.Errorf("parse wireguard address %q: %w", wgAddr, err)
	}

	localIP := net.IP(localAddr.Addr().AsSlice())

	u := &underlay{gateways: gws}

	for _, gw := range gws {
		tunDev, netDev, err := netstack.CreateNetTUN([]netip.Addr{localAddr.Addr()}, nil, wgTunnelMTU)
		if err != nil {
			u.Close()
			return nil, fmt.Errorf("create netstack tun for %s: %w", gw.iface, err)
		}

		logger := device.NewLogger(device.LogLevelError, fmt.Sprintf("(%s) ", gw.iface))
		dev := device.NewDevice(tunDev, conn.NewDefaultBind(), logger)

		uapi, err := buildUAPI(cfg, privHex, gw, podIP)
		if err != nil {
			dev.Close()
			u.Close()

			return nil, err
		}

		if err := dev.IpcSet(uapi); err != nil {
			dev.Close()
			u.Close()

			return nil, fmt.Errorf("configure %s: %w", gw.iface, err)
		}

		if err := dev.Up(); err != nil {
			dev.Close()
			u.Close()

			return nil, fmt.Errorf("bring up %s: %w", gw.iface, err)
		}

		udpConn, err := netDev.ListenUDP(&net.UDPAddr{IP: localIP, Port: cfg.VXLANPort})
		if err != nil {
			dev.Close()
			u.Close()

			return nil, fmt.Errorf("listen vxlan udp on %s: %w", gw.iface, err)
		}

		u.devices = append(u.devices, dev)
		u.conns = append(u.conns, udpConn)
	}

	return u, nil
}

// buildUAPI renders the wireguard-go UAPI configuration for a single gateway
// peer. Keys are converted from base64 to the hex encoding the UAPI expects.
// The demo pod's underlay address is added as a host allowed_ip so cryptokey
// routing carries VXLAN in both directions even when the pod's site pod CIDR is
// not covered by RouteCIDRs (allowed_ips gates outbound peer selection and
// filters the source of inbound packets, so the pod IP must be present or the
// overlay traffic is dropped).
func buildUAPI(cfg Config, privHex string, gw gateway, podIP string) (string, error) {
	peerHex, err := wgKeyBase64ToHex(gw.pubKey)
	if err != nil {
		return "", fmt.Errorf("gateway %s pubkey: %w", gw.iface, err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "private_key=%s\n", privHex)
	fmt.Fprintf(&b, "listen_port=%d\n", gw.port)
	fmt.Fprintf(&b, "public_key=%s\n", peerHex)
	fmt.Fprintf(&b, "endpoint=%s\n", gw.endpoint)
	fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", cfg.Keepalive)

	for _, cidr := range cfg.RouteCIDRs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
	}

	if strings.TrimSpace(podIP) != "" {
		addr, err := netip.ParseAddr(strings.TrimSpace(podIP))
		if err != nil {
			return "", fmt.Errorf("parse pod ip %q: %w", podIP, err)
		}

		fmt.Fprintf(&b, "allowed_ip=%s\n", netip.PrefixFrom(addr, addr.BitLen()))
	}

	return b.String(), nil
}

// outConn returns the UDP connection used for outbound VXLAN traffic.
func (u *underlay) outConn() *gonet.UDPConn {
	return u.conns[0]
}

// waitForHandshake polls the primary device until it reports a completed
// WireGuard handshake, or the timeout elapses.
func (u *underlay) waitForHandshake(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if conf, err := u.devices[0].IpcGet(); err == nil && handshakeEstablished(conf) {
			return true
		}

		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}

	return false
}

// handshakeEstablished reports whether a UAPI response shows a non-zero last
// handshake time for any peer.
func handshakeEstablished(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if !strings.HasPrefix(line, "last_handshake_time_sec=") {
			continue
		}

		if v := strings.TrimPrefix(line, "last_handshake_time_sec="); v != "" && v != "0" {
			return true
		}
	}

	return false
}

// transfer returns a human-readable per-interface WireGuard transfer summary,
// read from each device's UAPI, replacing "wg show <iface> transfer".
func (u *underlay) transfer() string {
	var sb strings.Builder

	for i, dev := range u.devices {
		rx, tx := int64(-1), int64(-1)

		if conf, err := dev.IpcGet(); err == nil {
			rx, tx = parseTransfer(conf)
		}

		fmt.Fprintf(&sb, "    %s: received %d B, sent %d B\n", u.gateways[i].iface, rx, tx)
	}

	return strings.TrimRight(sb.String(), "\n")
}

// parseTransfer extracts the summed rx_bytes/tx_bytes counters from a
// wireguard-go IpcGet response.
func parseTransfer(conf string) (rx, tx int64) {
	for _, line := range strings.Split(conf, "\n") {
		switch {
		case strings.HasPrefix(line, "rx_bytes="):
			if v, err := strconv.ParseInt(strings.TrimPrefix(line, "rx_bytes="), 10, 64); err == nil {
				rx += v
			}
		case strings.HasPrefix(line, "tx_bytes="):
			if v, err := strconv.ParseInt(strings.TrimPrefix(line, "tx_bytes="), 10, 64); err == nil {
				tx += v
			}
		}
	}

	return rx, tx
}

// Close tears down every WireGuard device and its listeners.
func (u *underlay) Close() {
	for _, c := range u.conns {
		if c != nil {
			_ = c.Close() //nolint:errcheck // best-effort close
		}
	}

	for _, dev := range u.devices {
		if dev != nil {
			dev.Close()
		}
	}

	u.conns = nil
	u.devices = nil
}

// overlay is the userspace VXLAN overlay endpoint: a gVisor stack with an
// Ethernet link endpoint (so ARP works) speaking IPv4/ICMP on the overlay
// subnet. Inner Ethernet frames are bridged to/from the WireGuard underlay with
// VXLAN encapsulation.
type overlay struct {
	stack    *stack.Stack
	ch       *channel.Endpoint
	localIP  net.IP
	remoteIP net.IP
	mac      tcpip.LinkAddress
	mtu      int
	// proxySrc, when non-nil, is the source IP the TFTP and forward proxies
	// bind their egress sockets to when dialing metalman on the host loopback.
	// It is the guest's real overlay lease IP, so metalman observes that IP as
	// the request source (instead of 127.0.0.1) and resolves the correct
	// Machine and boot lease.
	proxySrc net.IP
}

// newOverlay builds the overlay gVisor stack and its Ethernet link endpoint.
func newOverlay(cfg Config) (*overlay, error) {
	localIP := net.ParseIP(cfg.OverlayLocalIP).To4()
	if localIP == nil {
		return nil, fmt.Errorf("invalid overlay local ip %q", cfg.OverlayLocalIP)
	}

	remoteIP := net.ParseIP(cfg.OverlayRemoteIP).To4()
	if remoteIP == nil {
		return nil, fmt.Errorf("invalid overlay remote ip %q", cfg.OverlayRemoteIP)
	}

	mac, err := randomMAC()
	if err != nil {
		return nil, err
	}

	var proxySrc net.IP
	if strings.TrimSpace(cfg.ProxySourceIP) != "" {
		proxySrc = net.ParseIP(cfg.ProxySourceIP).To4()
		if proxySrc == nil {
			return nil, fmt.Errorf("invalid proxy source ip %q", cfg.ProxySourceIP)
		}
	}

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{gicmp.NewProtocol4, tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        true,
	})

	// gVisor leaves TCP SACK disabled by default. Over the high-latency
	// WireGuard overlay a single lost segment without SACK collapses the
	// congestion window and cripples throughput (each loss triggers a slow
	// recovery), which is fatal for large netboot transfers. Enable SACK and
	// widen the send/receive buffers so a single connection can keep the
	// bandwidth-delay product full.
	sackEnabled := tcpip.TCPSACKEnabled(true)
	if tcpErr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sackEnabled); tcpErr != nil {
		return nil, fmt.Errorf("enable tcp sack: %s", tcpErr)
	}

	sndBuf := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	if tcpErr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sndBuf); tcpErr != nil {
		return nil, fmt.Errorf("set tcp send buffer: %s", tcpErr)
	}

	rcvBuf := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	if tcpErr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcvBuf); tcpErr != nil {
		return nil, fmt.Errorf("set tcp receive buffer: %s", tcpErr)
	}

	ch := channel.New(1024, uint32(cfg.OverlayMTU+header.EthernetMinimumSize), mac)
	eth := ethernet.New(ch)

	if tcpErr := s.CreateNIC(overlayNICID, eth); tcpErr != nil {
		return nil, fmt.Errorf("create overlay nic: %s", tcpErr)
	}

	protoAddr := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(localIP),
			PrefixLen: cfg.OverlayPrefix,
		},
	}
	if tcpErr := s.AddProtocolAddress(overlayNICID, protoAddr, stack.AddressProperties{}); tcpErr != nil {
		return nil, fmt.Errorf("add overlay address: %s", tcpErr)
	}

	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: overlayNICID}})

	return &overlay{
		stack:    s,
		ch:       ch,
		localIP:  localIP,
		remoteIP: remoteIP,
		mac:      mac,
		mtu:      cfg.OverlayMTU,
		proxySrc: proxySrc,
	}, nil
}

// readOutbound blocks until the overlay stack emits an inner Ethernet frame (or
// the context is cancelled), returning the flattened frame bytes.
func (o *overlay) readOutbound(ctx context.Context) []byte {
	pkt := o.ch.ReadContext(ctx)
	if pkt.IsNil() {
		return nil
	}

	frame := pkt.ToBuffer()
	flat := frame.Flatten()

	pkt.DecRef()

	return flat
}

// injectInbound delivers a decapsulated inner Ethernet frame into the overlay
// stack.
func (o *overlay) injectInbound(frame []byte) {
	b := make([]byte, len(frame))
	copy(b, frame)

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(b),
	})
	o.ch.InjectInbound(0, pkt)
	pkt.DecRef()
}

// Close releases the overlay stack.
func (o *overlay) Close() {
	o.ch.Close()
	o.stack.Close()
}

// ping sends ICMP echo requests to the remote overlay IP over the overlay
// stack, printing each result, and returns the number of replies received.
func (o *overlay) ping(ctx context.Context, count int, timeout time.Duration) (int, error) {
	var wq waiter.Queue

	ep, tcpErr := o.stack.NewEndpoint(gicmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return 0, fmt.Errorf("create icmp endpoint: %s", tcpErr)
	}
	defer ep.Close()

	if tcpErr := ep.Bind(tcpip.FullAddress{NIC: overlayNICID, Addr: tcpip.AddrFromSlice(o.localIP)}); tcpErr != nil {
		return 0, fmt.Errorf("bind icmp endpoint: %s", tcpErr)
	}

	remote := tcpip.FullAddress{NIC: overlayNICID, Addr: tcpip.AddrFromSlice(o.remoteIP)}

	waitEntry, notifyCh := waiter.NewChannelEntry(waiter.EventIn)

	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	ident := os.Getpid() & 0xffff
	received := 0

	for seq := 0; seq < count; seq++ {
		if err := ctx.Err(); err != nil {
			return received, err
		}

		msg := xicmp.Message{
			Type: xipv4.ICMPTypeEcho,
			Code: 0,
			Body: &xicmp.Echo{
				ID:   ident,
				Seq:  seq,
				Data: []byte("playpen-overlay"),
			},
		}

		wb, err := msg.Marshal(nil)
		if err != nil {
			return received, fmt.Errorf("marshal icmp echo: %w", err)
		}

		start := time.Now()

		if _, tcpErr := ep.Write(bytes.NewReader(wb), tcpip.WriteOptions{To: &remote}); tcpErr != nil {
			fmt.Printf("  seq=%d send error: %s\n", seq, tcpErr)
			o.sleepBetween(ctx, seq, count)

			continue
		}

		if o.awaitReply(ctx, ep, notifyCh, seq, start, timeout) {
			received++
		}

		o.sleepBetween(ctx, seq, count)
	}

	return received, nil
}

// awaitReply waits for a matching echo reply for seq until the timeout elapses.
func (o *overlay) awaitReply(ctx context.Context, ep tcpip.Endpoint, notifyCh <-chan struct{}, seq int, start time.Time, timeout time.Duration) bool {
	deadline := start.Add(timeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Printf("  seq=%d timeout\n", seq)
			return false
		}

		timer := time.NewTimer(remaining)

		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
			fmt.Printf("  seq=%d timeout\n", seq)
			return false
		case <-notifyCh:
			timer.Stop()

			if o.readReply(ep, seq) {
				fmt.Printf("  seq=%d reply from %s time=%v\n", seq, o.remoteIP, time.Since(start).Round(time.Microsecond))
				return true
			}
		}
	}
}

// readReply reads one ICMP datagram from the endpoint and reports whether it is
// the echo reply for seq.
func (o *overlay) readReply(ep tcpip.Endpoint, seq int) bool {
	buf := make([]byte, 1500)
	w := tcpip.SliceWriter(buf)

	res, tcpErr := ep.Read(&w, tcpip.ReadOptions{})
	if tcpErr != nil {
		return false
	}

	parsed, err := xicmp.ParseMessage(1, buf[:res.Count])
	if err != nil {
		return false
	}

	if parsed.Type != xipv4.ICMPTypeEchoReply {
		return false
	}

	echo, ok := parsed.Body.(*xicmp.Echo)
	if !ok {
		return false
	}

	return echo.Seq == seq
}

// sleepBetween pauses roughly one second between pings, except after the last.
func (o *overlay) sleepBetween(ctx context.Context, seq, count int) {
	if seq >= count-1 {
		return
	}

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// randomMAC returns a locally-administered unicast MAC address.
func randomMAC() (tcpip.LinkAddress, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate mac: %w", err)
	}

	b[0] = (b[0] | 0x02) &^ 0x01 // locally administered, unicast

	return tcpip.LinkAddress(b[:]), nil
}

// dataplane wires the overlay stack to the WireGuard underlay with VXLAN
// encapsulation.
type dataplane struct {
	cfg     Config
	under   *underlay
	over    *overlay
	podAddr *net.UDPAddr
	relay   *dhcpRelay
	boot    *bootState
}

// newDataplane builds the full userspace dataplane: the WireGuard underlay, the
// overlay stack, and the VXLAN bridge between them targeting the pod IP. When
// the DHCP relay is enabled it is created here too, sharing the VXLAN send path.
func newDataplane(cfg Config, privHex, podIP string) (*dataplane, error) {
	podAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(podIP, strconv.Itoa(cfg.VXLANPort)))
	if err != nil {
		return nil, fmt.Errorf("resolve pod vxlan address: %w", err)
	}

	under, err := newUnderlay(cfg, privHex, podIP)
	if err != nil {
		return nil, err
	}

	over, err := newOverlay(cfg)
	if err != nil {
		under.Close()
		return nil, err
	}

	d := &dataplane{cfg: cfg, under: under, over: over, podAddr: podAddr, boot: &bootState{}}

	if cfg.dhcpEnabled() {
		relay, err := newDHCPRelay(cfg, net.HardwareAddr([]byte(over.mac)), d.sendToPod, d.boot)
		if err != nil {
			over.Close()
			under.Close()

			return nil, err
		}

		d.relay = relay
	}

	return d, nil
}

// sendToPod VXLAN-encapsulates an inner Ethernet frame and sends it to the pod
// over the primary WireGuard device. It is shared by the DHCP relay.
func (d *dataplane) sendToPod(frame []byte) {
	hdr := vxlanHeader(d.cfg.VNI)

	packet := make([]byte, 0, len(hdr)+len(frame))
	packet = append(packet, hdr...)
	packet = append(packet, frame...)

	if _, err := d.under.outConn().WriteTo(packet, d.podAddr); err != nil {
		fmt.Printf("dhcp relay: send to pod failed: %v\n", err)
	}
}

// run starts the bridge goroutines and blocks until the context is cancelled.
func (d *dataplane) run(ctx context.Context) {
	go d.pumpOverlayToPod(ctx)

	for _, c := range d.under.conns {
		go d.pumpPodToOverlay(ctx, c)
	}

	if d.relay != nil {
		go d.relay.run(ctx)
	}

	<-ctx.Done()
}

// pumpOverlayToPod reads inner Ethernet frames emitted by the overlay stack,
// VXLAN-encapsulates them, and sends them to the pod over the primary WireGuard
// device.
func (d *dataplane) pumpOverlayToPod(ctx context.Context) {
	for {
		frame := d.over.readOutbound(ctx)
		if frame == nil {
			return
		}

		d.sendToPod(frame)
	}
}

// pumpPodToOverlay reads VXLAN packets arriving on a WireGuard device, strips
// the VXLAN header, and injects the inner Ethernet frame into the overlay
// stack.
func (d *dataplane) pumpPodToOverlay(ctx context.Context, c *gonet.UDPConn) {
	buf := make([]byte, 65535)

	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			return
		}

		if n <= vxlanHeaderSize {
			continue
		}

		frame := buf[vxlanHeaderSize:n]

		if d.relay != nil && d.relay.handleFromPod(frame) {
			continue
		}

		d.over.injectInbound(frame)
	}
}

// Close tears down the overlay and underlay.
func (d *dataplane) Close() {
	if d.relay != nil {
		d.relay.close()
	}

	if d.over != nil {
		d.over.Close()
	}

	if d.under != nil {
		d.under.Close()
	}
}
