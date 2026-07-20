// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestWGAddress(t *testing.T) {
	cfg := DefaultConfig()

	got, err := cfg.wgAddress()
	if err != nil {
		t.Fatalf("wgAddress: %v", err)
	}

	if want := "100.123.0.1/32"; got != want {
		t.Fatalf("wgAddress = %q, want %q", got, want)
	}
}

func TestWGAddressInvalid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NodePodCIDR = "not-a-cidr"

	if _, err := cfg.wgAddress(); err == nil {
		t.Fatal("expected error for invalid pod cidr")
	}
}

func TestServerArgs(t *testing.T) {
	cfg := DefaultConfig()

	args, err := serverArgs(cfg, "jordan-playpen-abc12")
	if err != nil {
		t.Fatalf("serverArgs: %v", err)
	}

	if len(args) == 0 || args[0] != "server" {
		t.Fatalf("serverArgs[0] = %q, want \"server\"", args)
	}

	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--namespace jordan-testing",
		"--self-node-name jordan-playpen-abc12",
		"--vni 42",
		"--vxlan-port 4789",
		"--vxlan-interface pp-vx0",
		"--overlay-remote-ip 172.31.99.1",
		"--overlay-prefix 24",
		"--overlay-mtu 1230",
		"--node-pod-cidr 100.123.0.0/24",
		"--uplink eth0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("serverArgs missing %q\n%s", want, joined)
		}
	}
}

func TestServerArgsInvalidPodCIDR(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NodePodCIDR = "not-a-cidr"

	if _, err := serverArgs(cfg, "jordan-playpen-abc12"); err == nil {
		t.Fatal("expected error for invalid node pod cidr")
	}
}

func TestParsedForwards(t *testing.T) {
	t.Run("explicit and shorthand", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Forwards = []string{"8080:80", "9000", " 5000 : 5001 "}

		rules, err := cfg.parsedForwards()
		if err != nil {
			t.Fatalf("parsedForwards: %v", err)
		}

		want := []forwardRule{
			{overlayPort: 8080, loopbackPort: 80},
			{overlayPort: 9000, loopbackPort: 9000},
			{overlayPort: 5000, loopbackPort: 5001},
		}

		if len(rules) != len(want) {
			t.Fatalf("got %d rules, want %d: %+v", len(rules), len(want), rules)
		}

		for i := range want {
			if rules[i] != want[i] {
				t.Errorf("rule %d = %+v, want %+v", i, rules[i], want[i])
			}
		}
	})

	t.Run("empty slice is fine", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Forwards = nil

		rules, err := cfg.parsedForwards()
		if err != nil {
			t.Fatalf("parsedForwards: %v", err)
		}

		if len(rules) != 0 {
			t.Fatalf("got %d rules, want 0", len(rules))
		}
	})

	t.Run("invalid entries", func(t *testing.T) {
		for _, bad := range []string{"", "bad", "0:80", "80:0", "70000:1", "80:70000", "8080:"} {
			cfg := DefaultConfig()
			cfg.Forwards = []string{bad}

			if _, err := cfg.parsedForwards(); err == nil {
				t.Errorf("expected error for forward %q", bad)
			}
		}
	})

	t.Run("duplicate overlay port rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Forwards = []string{"8080:80", "8080:81"}

		if _, err := cfg.parsedForwards(); err == nil {
			t.Error("expected error for duplicate overlay port")
		}
	})
}

func TestWGKeyBase64ToHex(t *testing.T) {
	// 32 zero bytes -> 64 hex zeros.
	zero := base64.StdEncoding.EncodeToString(make([]byte, 32))

	got, err := wgKeyBase64ToHex(zero)
	if err != nil {
		t.Fatalf("wgKeyBase64ToHex: %v", err)
	}

	if want := strings.Repeat("0", 64); got != want {
		t.Fatalf("wgKeyBase64ToHex = %q, want %q", got, want)
	}

	// A known gateway key round-trips to 64 hex chars.
	got, err = wgKeyBase64ToHex("jmiGvW/EIsSYMDhq+veuuiJgdsg2lGWP3TA8wuilJkg=")
	if err != nil {
		t.Fatalf("wgKeyBase64ToHex real key: %v", err)
	}

	if len(got) != 64 {
		t.Fatalf("hex key length = %d, want 64", len(got))
	}
}

func TestWGKeyBase64ToHexInvalid(t *testing.T) {
	// Not base64.
	if _, err := wgKeyBase64ToHex("not valid base64!!!"); err == nil {
		t.Error("expected error for non-base64 key")
	}

	// Valid base64 but wrong length.
	short := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if _, err := wgKeyBase64ToHex(short); err == nil {
		t.Error("expected error for wrong-length key")
	}
}

func TestVXLANHeader(t *testing.T) {
	h := vxlanHeader(42)

	if len(h) != vxlanHeaderSize {
		t.Fatalf("vxlan header length = %d, want %d", len(h), vxlanHeaderSize)
	}

	if h[0] != 0x08 {
		t.Errorf("vxlan I flag not set: got 0x%02x", h[0])
	}

	// VNI 42 = 0x00002A occupies bytes 4..6.
	if h[4] != 0x00 || h[5] != 0x00 || h[6] != 0x2a {
		t.Errorf("vxlan vni bytes = %02x %02x %02x, want 00 00 2a", h[4], h[5], h[6])
	}

	if h[7] != 0x00 {
		t.Errorf("vxlan reserved byte 7 = 0x%02x, want 0", h[7])
	}
}

func TestParseTransfer(t *testing.T) {
	conf := strings.Join([]string{
		"private_key=00",
		"public_key=ff",
		"rx_bytes=100",
		"tx_bytes=200",
		"public_key=ee",
		"rx_bytes=50",
		"tx_bytes=25",
		"errno=0",
	}, "\n")

	rx, tx := parseTransfer(conf)
	if rx != 150 {
		t.Errorf("rx = %d, want 150", rx)
	}

	if tx != 225 {
		t.Errorf("tx = %d, want 225", tx)
	}
}

func TestHandshakeEstablished(t *testing.T) {
	if handshakeEstablished("last_handshake_time_sec=0\nlast_handshake_time_nsec=0") {
		t.Error("zero handshake time should not count as established")
	}

	if !handshakeEstablished("public_key=ff\nlast_handshake_time_sec=1700000000") {
		t.Error("non-zero handshake time should count as established")
	}
}

func TestBuildUAPI(t *testing.T) {
	cfg := DefaultConfig()
	gw := cfg.gateways()[0]

	uapi, err := buildUAPI(cfg, strings.Repeat("0", 64), gw, "100.100.0.13")
	if err != nil {
		t.Fatalf("buildUAPI: %v", err)
	}

	for _, want := range []string{
		"private_key=" + strings.Repeat("0", 64),
		"listen_port=51900",
		"public_key=",
		"endpoint=20.104.49.219:51820",
		"persistent_keepalive_interval=15",
		"allowed_ip=100.125.0.0/16",
		"allowed_ip=10.224.0.0/12",
		"allowed_ip=100.100.0.13/32",
	} {
		if !strings.Contains(uapi, want) {
			t.Errorf("uapi missing %q\n%s", want, uapi)
		}
	}
}

func TestBuildUAPIPodIP(t *testing.T) {
	cfg := DefaultConfig()
	gw := cfg.gateways()[0]

	t.Run("empty pod ip adds no host route", func(t *testing.T) {
		uapi, err := buildUAPI(cfg, strings.Repeat("0", 64), gw, "")
		if err != nil {
			t.Fatalf("buildUAPI: %v", err)
		}

		if strings.Contains(uapi, "/32") {
			t.Errorf("empty pod ip should not add a host allowed_ip\n%s", uapi)
		}
	})

	t.Run("invalid pod ip errors", func(t *testing.T) {
		if _, err := buildUAPI(cfg, strings.Repeat("0", 64), gw, "not-an-ip"); err == nil {
			t.Error("expected error for invalid pod ip")
		}
	})
}

// buildRequestFrame wraps a DHCP payload in UDP/IPv4/Ethernet headers destined
// for the DHCP server port, mirroring what a DHCP client on the pod emits onto
// the overlay. It is the request-side analogue of buildReplyFrame.
func buildRequestFrame(t *testing.T, payload []byte, srcMAC net.HardwareAddr) []byte {
	t.Helper()

	const (
		ethLen = header.EthernetMinimumSize
		ipLen  = header.IPv4MinimumSize
		udpLen = header.UDPMinimumSize
	)

	frame := make([]byte, ethLen+ipLen+udpLen+len(payload))

	srcAddr := tcpip.AddrFrom4Slice([]byte{0, 0, 0, 0})
	dstAddr := tcpip.AddrFrom4Slice([]byte{255, 255, 255, 255})

	header.Ethernet(frame[:ethLen]).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(srcMAC),
		DstAddr: tcpip.LinkAddress(broadcastMAC),
		Type:    header.IPv4ProtocolNumber,
	})

	udpTotal := uint16(udpLen + len(payload))
	udp := header.UDP(frame[ethLen+ipLen:])
	udp.Encode(&header.UDPFields{SrcPort: dhcpClientPort, DstPort: dhcpServerPort, Length: udpTotal})
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

// ipSrc and ipDst copy the addresses out of an IPv4 header. AsSlice has a
// pointer receiver, so the address must be stored in an addressable local first.
func ipSrc(ip header.IPv4) []byte {
	a := ip.SourceAddress()
	return a.AsSlice()
}

func ipDst(ip header.IPv4) []byte {
	a := ip.DestinationAddress()
	return a.AsSlice()
}

func TestParseDHCPRequest(t *testing.T) {
	clientMAC := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}

	disc, err := dhcpv4.NewDiscovery(clientMAC, dhcpv4.WithTransactionID(dhcpv4.TransactionID{0xde, 0xad, 0xbe, 0xef}))
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	frame := buildRequestFrame(t, disc.ToBytes(), clientMAC)

	msg, gotMAC, ok := parseDHCPRequest(frame)
	if !ok {
		t.Fatal("parseDHCPRequest returned ok=false for a valid BOOTREQUEST")
	}

	if msg.OpCode != dhcpv4.OpcodeBootRequest {
		t.Errorf("opcode = %v, want BootRequest", msg.OpCode)
	}

	if msg.TransactionID != (dhcpv4.TransactionID{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("xid = %v, want deadbeef", msg.TransactionID)
	}

	if gotMAC.String() != clientMAC.String() {
		t.Errorf("client mac = %s, want %s", gotMAC, clientMAC)
	}

	t.Run("non-dhcp frames rejected", func(t *testing.T) {
		if _, _, ok := parseDHCPRequest([]byte{0x00, 0x01}); ok {
			t.Error("short frame accepted")
		}

		// A frame whose UDP destination is not the server port is not a request.
		reply := buildReplyFrame(disc.ToBytes(), clientMAC, clientMAC, net.IP{172, 31, 99, 2})
		if _, _, ok := parseDHCPRequest(reply); ok {
			t.Error("reply frame (udp dst 68) accepted as request")
		}
	})
}

func TestBuildReplyFrame(t *testing.T) {
	clientMAC := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	serverMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	srcIP := net.IP{172, 31, 99, 2}

	req, err := dhcpv4.NewDiscovery(clientMAC, dhcpv4.WithTransactionID(dhcpv4.TransactionID{1, 2, 3, 4}))
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	reply, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		t.Fatalf("NewReplyFromRequest: %v", err)
	}

	payload := reply.ToBytes()
	frame := buildReplyFrame(payload, clientMAC, serverMAC, srcIP)

	eth := header.Ethernet(frame)
	if eth.Type() != header.IPv4ProtocolNumber {
		t.Fatalf("ethertype = %v, want IPv4", eth.Type())
	}

	if got := net.HardwareAddr(eth.DestinationAddress()); got.String() != clientMAC.String() {
		t.Errorf("dst mac = %s, want %s", got, clientMAC)
	}

	if got := net.HardwareAddr(eth.SourceAddress()); got.String() != serverMAC.String() {
		t.Errorf("src mac = %s, want %s", got, serverMAC)
	}

	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(frame) - header.EthernetMinimumSize) {
		t.Fatal("ipv4 header not valid")
	}

	// A correct IPv4 checksum makes the one's-complement sum of the header 0xffff.
	if sum := checksum.Checksum(ip[:header.IPv4MinimumSize], 0); sum != 0xffff {
		t.Errorf("ipv4 checksum invalid: header sum = %#04x, want 0xffff", sum)
	}

	if got := net.IP(ipSrc(ip)); !got.Equal(srcIP) {
		t.Errorf("ip src = %s, want %s", got, srcIP)
	}

	if got := net.IP(ipDst(ip)); !got.Equal(net.IP{255, 255, 255, 255}) {
		t.Errorf("ip dst = %s, want 255.255.255.255", got)
	}

	udp := header.UDP(ip.Payload())
	if udp.SourcePort() != dhcpServerPort || udp.DestinationPort() != dhcpClientPort {
		t.Errorf("udp ports = %d->%d, want %d->%d", udp.SourcePort(), udp.DestinationPort(), dhcpServerPort, dhcpClientPort)
	}

	// Validate the UDP checksum over the pseudo-header + UDP header + payload.
	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber,
		ip.SourceAddress(), ip.DestinationAddress(), udp.Length())
	xsum = checksum.Checksum(udp.Payload(), xsum)

	if sum := udp.CalculateChecksum(xsum); sum != 0xffff {
		t.Errorf("udp checksum invalid: sum = %#04x, want 0xffff", sum)
	}

	if got := udp.Payload(); string(got) != string(payload) {
		t.Error("udp payload does not round-trip the dhcp reply")
	}
}

func TestStampRelay(t *testing.T) {
	giaddr := net.IP{10, 0, 0, 9}

	t.Run("sets giaddr and increments hops when unset", func(t *testing.T) {
		msg, err := dhcpv4.NewDiscovery(net.HardwareAddr{0x02, 0, 0, 0, 0, 1})
		if err != nil {
			t.Fatalf("NewDiscovery: %v", err)
		}

		stampRelay(msg, giaddr)

		if !msg.GatewayIPAddr.Equal(giaddr) {
			t.Errorf("giaddr = %s, want %s", msg.GatewayIPAddr, giaddr)
		}

		if msg.HopCount != 1 {
			t.Errorf("hop count = %d, want 1", msg.HopCount)
		}
	})

	t.Run("preserves an existing giaddr", func(t *testing.T) {
		existing := net.IP{192, 168, 5, 1}

		msg, err := dhcpv4.NewDiscovery(net.HardwareAddr{0x02, 0, 0, 0, 0, 2})
		if err != nil {
			t.Fatalf("NewDiscovery: %v", err)
		}

		msg.GatewayIPAddr = existing

		stampRelay(msg, giaddr)

		if !msg.GatewayIPAddr.Equal(existing) {
			t.Errorf("giaddr = %s, want preserved %s", msg.GatewayIPAddr, existing)
		}

		if msg.HopCount != 1 {
			t.Errorf("hop count = %d, want 1", msg.HopCount)
		}
	})
}

func TestDHCPConfigValidation(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		cfg := DefaultConfig()
		if cfg.dhcpEnabled() {
			t.Fatal("dhcp should be disabled by default")
		}

		if err := cfg.validateDHCP(); err != nil {
			t.Fatalf("validateDHCP with relay off: %v", err)
		}
	})

	t.Run("relay on requires a server", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DHCPRelayPort = 67

		if err := cfg.validateDHCP(); err == nil {
			t.Fatal("expected error when relay enabled without --dhcp-server")
		}
	})

	t.Run("invalid giaddr rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DHCPRelayPort = 6767
		cfg.DHCPServer = "10.0.0.1"
		cfg.DHCPGiaddr = "not-an-ip"

		if err := cfg.validateDHCP(); err == nil {
			t.Fatal("expected error for invalid --dhcp-giaddr")
		}
	})

	t.Run("relay port out of range rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DHCPRelayPort = 70000
		cfg.DHCPServer = "10.0.0.1"

		if err := cfg.validateDHCP(); err == nil {
			t.Fatal("expected error for out-of-range --dhcp-relay-port")
		}
	})

	t.Run("valid config with default server port", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DHCPRelayPort = 6767
		cfg.DHCPServer = "10.0.0.1"

		if err := cfg.validateDHCP(); err != nil {
			t.Fatalf("validateDHCP: %v", err)
		}

		addr, err := cfg.dhcpServerAddr()
		if err != nil {
			t.Fatalf("dhcpServerAddr: %v", err)
		}

		if addr.Port != dhcpServerPort {
			t.Errorf("default server port = %d, want %d", addr.Port, dhcpServerPort)
		}
	})
}

func TestServerArgsVM(t *testing.T) {
	t.Run("vm and redfish flags always present", func(t *testing.T) {
		cfg := DefaultConfig()

		args, err := serverArgs(cfg, "jordan-playpen-abc12")
		if err != nil {
			t.Fatalf("serverArgs: %v", err)
		}

		joined := strings.Join(args, " ")

		for _, want := range []string{
			"--vm-memory 512",
			"--vm-cpus 1",
			"--vm-mac 52:54:00:12:34:56",
			"--vm-disk-size 20",
			"--bridge-interface pp-br0",
			"--tap-interface pp-tap0",
			"--redfish-port 8443",
			"--redfish-username admin",
			"--redfish-password password",
			"--redfish-device-id 1",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("serverArgs missing %q\n%s", want, joined)
			}
		}

		// The --vm toggle was removed; a VM is always provisioned.
		for _, forbidden := range []string{"--vm ", "--vm-image"} {
			if strings.Contains(joined+" ", forbidden) {
				t.Errorf("did not expect %q in serverArgs: %s", forbidden, joined)
			}
		}
	})
}

func TestVMConfig(t *testing.T) {
	cfg := DefaultConfig()

	vc := vmConfig(cfg, "/tmp/serial.log")

	if vc.Cpus.BootVcpus != 1 || vc.Cpus.MaxVcpus != 1 {
		t.Errorf("cpus = %+v, want boot/max 1", vc.Cpus)
	}

	if want := int64(512) * 1024 * 1024; vc.Memory.Size != want {
		t.Errorf("memory size = %d, want %d", vc.Memory.Size, want)
	}

	if vc.Payload.Firmware != chFirmware {
		t.Errorf("firmware = %q, want %q", vc.Payload.Firmware, chFirmware)
	}

	if len(vc.Net) != 1 || vc.Net[0].Tap != "pp-tap0" || vc.Net[0].Mac != "52:54:00:12:34:56" {
		t.Errorf("net = %+v, want single tap pp-tap0 mac 52:54:00:12:34:56", vc.Net)
	}

	if vc.Serial.Mode != "File" || vc.Serial.File != "/tmp/serial.log" {
		t.Errorf("serial = %+v, want File /tmp/serial.log", vc.Serial)
	}

	if vc.Console.Mode != "Off" {
		t.Errorf("console mode = %q, want Off", vc.Console.Mode)
	}

	if len(vc.Disks) != 1 || vc.Disks[0].Path != defaultVMDiskPath {
		t.Errorf("disks = %+v, want single disk %q", vc.Disks, defaultVMDiskPath)
	}

	if vc.Tpm == nil || vc.Tpm.Socket != tpmSocketPath() {
		t.Errorf("tpm = %+v, want socket %q", vc.Tpm, tpmSocketPath())
	}
}

func TestVMConfigDiskless(t *testing.T) {
	cfg := DefaultConfig()
	cfg.VMDiskSizeGiB = 0

	vc := vmConfig(cfg, "/tmp/serial.log")

	if len(vc.Disks) != 0 {
		t.Errorf("disks = %+v, want none for a diskless guest", vc.Disks)
	}
}

func TestValidateVM(t *testing.T) {
	t.Run("valid config accepted with explicit tftp", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.TFTPServer = "172.31.99.5"

		if err := cfg.validateVM(); err != nil {
			t.Fatalf("validateVM: %v", err)
		}
	})

	t.Run("tftp optional (no dhcp/tftp accepted)", func(t *testing.T) {
		cfg := DefaultConfig()

		if err := cfg.validateVM(); err != nil {
			t.Fatalf("validateVM with no tftp/dhcp should be soft: %v", err)
		}
	})

	t.Run("tftp server defaults to dhcp server host", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DHCPServer = "172.31.99.5:67"

		if err := cfg.validateVM(); err != nil {
			t.Fatalf("validateVM with dhcp-derived tftp: %v", err)
		}
	})

	t.Run("invalid MAC rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.VMMAC = "not-a-mac"

		if err := cfg.validateVM(); err == nil {
			t.Fatal("expected error for invalid --vm-mac")
		}
	})

	t.Run("non-positive resources rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.VMCPUs = 0

		if err := cfg.validateVM(); err == nil {
			t.Fatal("expected error for zero --vm-cpus")
		}
	})

	t.Run("negative disk size rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.VMDiskSizeGiB = -1

		if err := cfg.validateVM(); err == nil {
			t.Fatal("expected error for negative --vm-disk-size")
		}
	})

	t.Run("diskless (zero disk size) accepted", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.VMDiskSizeGiB = 0

		if err := cfg.validateVM(); err != nil {
			t.Fatalf("validateVM with zero disk size: %v", err)
		}
	})
}

func TestValidateRedfish(t *testing.T) {
	t.Run("defaults accepted", func(t *testing.T) {
		cfg := DefaultConfig()
		if err := cfg.validateRedfish(); err != nil {
			t.Fatalf("validateRedfish default: %v", err)
		}
	})

	t.Run("empty username rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RedfishUsername = ""

		if err := cfg.validateRedfish(); err == nil {
			t.Fatal("expected error for empty --redfish-username")
		}
	})

	t.Run("out-of-range port rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RedfishPort = 0

		if err := cfg.validateRedfish(); err == nil {
			t.Fatal("expected error for zero --redfish-port")
		}
	})
}

func TestValidateSite(t *testing.T) {
	t.Run("defaults accepted", func(t *testing.T) {
		cfg := DefaultConfig()
		if err := cfg.validateSite(); err != nil {
			t.Fatalf("validateSite default: %v", err)
		}
	})

	t.Run("node internal IP outside site node cidr rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.NodeInternalIP = "192.0.2.1"

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for --node-internal-ip outside --site-node-cidr")
		}
	})

	t.Run("node pod cidr outside site pod cidr rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.NodePodCIDR = "100.200.0.0/24"

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for --node-pod-cidr outside --site-pod-cidr")
		}
	})

	t.Run("node pod cidr wider than site pod cidr rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.NodePodCIDR = "100.123.0.0/8"

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for --node-pod-cidr wider than --site-pod-cidr")
		}
	})

	t.Run("empty gateway pools rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.GatewayPools = nil

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for empty --gateway-pools")
		}
	})

	t.Run("blank gateway pool rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.GatewayPools = []string{" "}

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for blank --gateway-pools entry")
		}
	})

	t.Run("invalid site node cidr rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SiteNodeCIDR = "not-a-cidr"

		if err := cfg.validateSite(); err == nil {
			t.Fatal("expected error for invalid --site-node-cidr")
		}
	})
}

func TestNormalizeArch(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"amd64", "amd64"},
		{"x86", "amd64"},
		{"x86_64", "amd64"},
		{"x64", "amd64"},
		{"AMD64", "amd64"},
		{" amd64 ", "amd64"},
		{"arm64", "arm64"},
		{"arm", "arm64"},
		{"aarch64", "arm64"},
		{"ARM64", "arm64"},
	} {
		got, err := normalizeArch(tc.in)
		if err != nil {
			t.Errorf("normalizeArch(%q): unexpected error: %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("normalizeArch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "riscv64", "ppc64le", "x86-64"} {
		if _, err := normalizeArch(bad); err == nil {
			t.Errorf("normalizeArch(%q): expected error", bad)
		}
	}
}

func TestValidateArch(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Arch != "amd64" {
		t.Fatalf("default arch = %q, want amd64", cfg.Arch)
	}

	if err := cfg.validateArch(); err != nil {
		t.Fatalf("validateArch default: %v", err)
	}

	cfg.Arch = "sparc"
	if err := cfg.validateArch(); err == nil {
		t.Fatal("expected error for unsupported arch")
	}
}

func TestKVMNodeLabel(t *testing.T) {
	t.Run("key=value", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.KVMNodeLabel = "example.com/kvm=yes"

		key, value, err := cfg.kvmNodeLabel()
		if err != nil {
			t.Fatalf("kvmNodeLabel: %v", err)
		}

		if key != "example.com/kvm" || value != "yes" {
			t.Fatalf("kvmNodeLabel() = %q=%q, want example.com/kvm=yes", key, value)
		}
	})

	t.Run("bare key implies true", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.KVMNodeLabel = "example.com/kvm"

		key, value, err := cfg.kvmNodeLabel()
		if err != nil {
			t.Fatalf("kvmNodeLabel: %v", err)
		}

		if key != "example.com/kvm" || value != "true" {
			t.Fatalf("kvmNodeLabel() = %q=%q, want example.com/kvm=true", key, value)
		}
	})

	t.Run("default", func(t *testing.T) {
		cfg := DefaultConfig()

		key, value, err := cfg.kvmNodeLabel()
		if err != nil {
			t.Fatalf("kvmNodeLabel: %v", err)
		}

		if key != "playpen.unbounded-cloud.io/kvm" || value != "true" {
			t.Fatalf("kvmNodeLabel() = %q=%q, want playpen.unbounded-cloud.io/kvm=true", key, value)
		}
	})

	t.Run("empty rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.KVMNodeLabel = ""

		if _, _, err := cfg.kvmNodeLabel(); err == nil {
			t.Fatal("expected error for empty --kvm-node-label")
		}
	})
}

func TestPodNodeSelector(t *testing.T) {
	t.Run("arch by default", func(t *testing.T) {
		cfg := DefaultConfig()

		sel, err := cfg.podNodeSelector()
		if err != nil {
			t.Fatalf("podNodeSelector: %v", err)
		}

		if sel[ArchLabelKey] != "amd64" {
			t.Fatalf("podNodeSelector() = %v, want %s: amd64", sel, ArchLabelKey)
		}
	})

	t.Run("arm64 normalized from aarch64", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Arch = "aarch64"

		sel, err := cfg.podNodeSelector()
		if err != nil {
			t.Fatalf("podNodeSelector: %v", err)
		}

		if sel[ArchLabelKey] != "arm64" {
			t.Fatalf("podNodeSelector() arch = %q, want arm64", sel[ArchLabelKey])
		}
	})

	t.Run("always adds kvm label", func(t *testing.T) {
		cfg := DefaultConfig()

		sel, err := cfg.podNodeSelector()
		if err != nil {
			t.Fatalf("podNodeSelector: %v", err)
		}

		if sel[ArchLabelKey] != "amd64" {
			t.Fatalf("podNodeSelector() missing arch: %v", sel)
		}

		if sel["playpen.unbounded-cloud.io/kvm"] != "true" {
			t.Fatalf("podNodeSelector() missing kvm label: %v", sel)
		}
	})

	t.Run("explicit pod-node bypasses selector", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.PodNode = "some-node"

		sel, err := cfg.podNodeSelector()
		if err != nil {
			t.Fatalf("podNodeSelector: %v", err)
		}

		if sel != nil {
			t.Fatalf("podNodeSelector() = %v, want nil when --pod-node set", sel)
		}
	})

	t.Run("invalid arch rejected", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Arch = "sparc"

		if _, err := cfg.podNodeSelector(); err == nil {
			t.Fatal("expected error for unsupported arch")
		}
	})
}
