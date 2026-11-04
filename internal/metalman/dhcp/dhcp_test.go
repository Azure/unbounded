// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dhcp

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"oras.land/oras-go/v2/registry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

func TestDHCPHandler(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
				Gateway:    "10.0.1.1",
				DNS:        []string{"1.1.1.1", "8.8.8.8"},
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}

	if !resp.YourIPAddr.Equal(net.ParseIP("10.0.1.10")) {
		t.Errorf("expected YourIP 10.0.1.10, got %s", resp.YourIPAddr)
	}

	if mask := resp.SubnetMask(); !net.IP(mask).Equal(net.ParseIP("255.255.255.0")) {
		t.Errorf("expected subnet 255.255.255.0, got %s", net.IP(mask))
	}

	routers := resp.Router()
	if len(routers) == 0 || !routers[0].Equal(net.ParseIP("10.0.1.1")) {
		t.Errorf("expected gateway 10.0.1.1, got %v", routers)
	}

	dnsServers := resp.DNS()
	if len(dnsServers) != 2 {
		t.Fatalf("expected 2 DNS servers, got %d", len(dnsServers))
	}

	if !dnsServers[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Errorf("expected DNS 1.1.1.1, got %s", dnsServers[0])
	}

	if !dnsServers[1].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("expected DNS 8.8.8.8, got %s", dnsServers[1])
	}

	if resp.GetOneOption(dhcpv4.OptionTFTPServerName) != nil {
		t.Error("expected no TFTP server option for non-PXE node")
	}

	if resp.GetOneOption(dhcpv4.OptionBootfileName) != nil {
		t.Error("expected no bootfile option for non-PXE node")
	}
}

func TestDHCPHandlerPXE(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f1")
	serverIP := net.ParseIP("10.0.1.254").To4()

	machineImageRef := "ghcr.io/test/ubuntu-24-04:v1"
	netbootImageRef := "ghcr.io/test/netboot:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-02"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: machineImageRef, NetbootImage: netbootImageRef, DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f1",
				IPv4:       "10.0.1.11",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	// Push an image with metadata for the netboot image ref.
	images := newTestImages(t, netbootImageRef, "", "dhcpBootImageName: shimx64.efi\n")

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
		Images:    images,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.11"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}

	if !resp.YourIPAddr.Equal(net.ParseIP("10.0.1.11")) {
		t.Errorf("expected YourIP 10.0.1.11, got %s", resp.YourIPAddr)
	}

	tftpServer := resp.TFTPServerName()
	if tftpServer != serverIP.String() {
		t.Errorf("expected TFTP server %s, got %s", serverIP, tftpServer)
	}

	bootfile := resp.BootFileNameOption()
	if bootfile != "shimx64.efi" {
		t.Errorf("expected bootfile shimx64.efi, got %s", bootfile)
	}

	if !resp.ServerIPAddr.Equal(serverIP) {
		t.Errorf("expected next-server %s, got %s", serverIP, resp.ServerIPAddr)
	}
}

func TestDHCPHandlerPXEUsesDefaultNetbootImage(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f2")
	serverIP := net.ParseIP("10.0.1.254").To4()
	netbootImageRef := "ghcr.io/test/default-netboot:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-default-netboot"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Architecture: v1alpha3.PXEArchitectureARM64,
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:f2",
					IPv4:       "10.0.1.12",
					SubnetMask: "255.255.255.0",
				}},
			},
		},
	}

	images := newTestImages(t, netbootImageRef, v1alpha3.PXEArchitectureARM64, "dhcpBootImageName: shimaa64.efi\n")

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface:         "eth0",
		Reader:            reader,
		ServerIP:          serverIP,
		Images:            images,
		DefaultNetbootRef: netbootImageRef,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.12"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if bootfile := resp.BootFileNameOption(); bootfile != "shimaa64.efi" {
		t.Errorf("expected bootfile shimaa64.efi, got %s", bootfile)
	}
}

func TestDHCPHandlerHTTPBootSuppressesPXEBootOptions(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f3")
	serverIP := net.ParseIP("10.0.1.254").To4()
	netbootImageRef := "ghcr.io/test/netboot:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-boot"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				NetbootImage: netbootImageRef,
				BootProtocol: v1alpha3.PXEBootProtocolHTTP,
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:f3",
					IPv4:       "10.0.1.13",
					SubnetMask: "255.255.255.0",
				}},
			},
		},
	}

	images := newTestImages(t, netbootImageRef, "", "dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n")

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
		Images:    images,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.13"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if tftpServer := resp.TFTPServerName(); tftpServer != "" {
		t.Errorf("expected no TFTP server for HTTP boot, got %s", tftpServer)
	}

	if bootfile := resp.BootFileNameOption(); bootfile != "" {
		t.Errorf("expected no bootfile for HTTP boot, got %s", bootfile)
	}

	if !resp.YourIPAddr.Equal(net.ParseIP("10.0.1.13")) {
		t.Errorf("expected YourIP 10.0.1.13, got %s", resp.YourIPAddr)
	}
}

func TestDHCPHandlerHTTPBootClientGetsHTTPBootURL(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f4")
	serverIP := net.ParseIP("10.0.1.254").To4()
	netbootImageRef := "ghcr.io/test/netboot:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-client-boot"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				NetbootImage: netbootImageRef,
				BootProtocol: v1alpha3.PXEBootProtocolHTTP,
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:f4",
					IPv4:       "10.0.1.14",
					SubnetMask: "255.255.255.0",
				}},
			},
		},
	}

	images := newTestImages(t, netbootImageRef, "", "dhcpBootImageName: pxe-shimx64.efi\nhttpBootPath: http/shimx64.efi\n")

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
		Images:    images,
		ServeURL:  "http://10.0.1.254:8880/base/",
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	discover.UpdateOption(dhcpv4.OptClassIdentifier("HTTPClient:Arch:00016:UNDI:003016"))

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.14"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if tftpServer := resp.TFTPServerName(); tftpServer != "" {
		t.Errorf("expected no TFTP server for HTTP boot, got %s", tftpServer)
	}

	if bootfile := resp.BootFileNameOption(); bootfile != "http://10.0.1.254:8880/base/http/shimx64.efi" {
		t.Errorf("expected HTTP boot URL, got %s", bootfile)
	}

	if resp.GetOneOption(dhcpv4.OptionTFTPServerName) != nil {
		t.Error("expected no TFTP server option for HTTP boot")
	}

	if !resp.YourIPAddr.Equal(net.ParseIP("10.0.1.14")) {
		t.Errorf("expected YourIP 10.0.1.14, got %s", resp.YourIPAddr)
	}
}

func TestDHCPHandlerVirtualMediaSuppressesBootHints(t *testing.T) {
	mac, err := net.ParseMAC("aa:bb:cc:dd:ee:f5")
	if err != nil {
		t.Fatal(err)
	}

	serverIP := net.ParseIP("10.0.1.254").To4()
	netbootImageRef := "ghcr.io/test/netboot:v1"
	images := newTestImages(t, netbootImageRef, "", "dhcpBootImageName: pxe-shimx64.efi\nhttpBootPath: http/shimx64.efi\n")

	for _, imageSource := range []string{"machine", "default"} {
		t.Run(imageSource, func(t *testing.T) {
			node := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-virtual-media"},
				Spec: v1alpha3.MachineSpec{
					Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
						BootProtocol: v1alpha3.PXEBootProtocolVirtualMedia,
						DHCPLeases: []v1alpha3.DHCPLease{{
							MAC: mac.String(), IPv4: "10.0.1.15", SubnetMask: "255.255.255.0",
							Gateway: "10.0.1.1", DNS: []string{"1.1.1.1"},
						}},
					}},
				},
			}
			if imageSource == "machine" {
				node.Spec.Host.Netboot.NetbootImage = netbootImageRef
			}

			srv := &Server{
				Interface: "eth0", Reader: newFakeReader(t, node), ServerIP: serverIP,
				Images: images, ServeURL: "http://10.0.1.254:8880/base/",
				DefaultNetbootRef: netbootImageRef,
			}

			for _, class := range []string{"PXEClient:Arch:00007:UNDI:003016", "HTTPClient:Arch:00016:UNDI:003016"} {
				t.Run(class, func(t *testing.T) {
					for _, exchange := range []struct {
						request dhcpv4.MessageType
						reply   dhcpv4.MessageType
					}{
						{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeOffer},
						{dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeAck},
					} {
						t.Run(exchange.request.String(), func(t *testing.T) {
							request, err := dhcpv4.NewDiscovery(mac,
								dhcpv4.WithOption(dhcpv4.OptClassIdentifier(class)),
								dhcpv4.WithOption(dhcpv4.OptMessageType(exchange.request)))
							if err != nil {
								t.Fatal(err)
							}

							conn := &fakePacketConn{}
							srv.handler(conn, &net.UDPAddr{IP: net.ParseIP("10.0.1.15"), Port: 68}, request)

							if conn.written == nil {
								t.Fatal("expected DHCP response for VirtualMedia")
							}

							resp, err := dhcpv4.FromBytes(conn.written)
							if err != nil {
								t.Fatal(err)
							}

							if resp.MessageType() != exchange.reply {
								t.Errorf("expected %s, got %s", exchange.reply, resp.MessageType())
							}

							if !resp.YourIPAddr.Equal(net.ParseIP("10.0.1.15")) {
								t.Errorf("expected YourIP 10.0.1.15, got %s", resp.YourIPAddr)
							}

							if !resp.ServerIdentifier().Equal(serverIP) {
								t.Errorf("expected DHCP server identifier %s, got %s", serverIP, resp.ServerIdentifier())
							}

							if mask := resp.SubnetMask(); !net.IP(mask).Equal(net.ParseIP("255.255.255.0")) {
								t.Errorf("expected subnet mask 255.255.255.0, got %s", net.IP(mask))
							}

							if routers := resp.Router(); len(routers) != 1 || !routers[0].Equal(net.ParseIP("10.0.1.1")) {
								t.Errorf("expected gateway 10.0.1.1, got %v", routers)
							}

							if dns := resp.DNS(); len(dns) != 1 || !dns[0].Equal(net.ParseIP("1.1.1.1")) {
								t.Errorf("expected DNS 1.1.1.1, got %v", dns)
							}

							if resp.GetOneOption(dhcpv4.OptionTFTPServerName) != nil {
								t.Error("expected no TFTP server option for VirtualMedia")
							}

							if resp.GetOneOption(dhcpv4.OptionBootfileName) != nil {
								t.Error("expected no bootfile option for VirtualMedia")
							}

							if resp.BootFileName != "" {
								t.Errorf("expected empty BOOTP bootfile for VirtualMedia, got %q", resp.BootFileName)
							}

							if !resp.ServerIPAddr.IsUnspecified() {
								t.Errorf("expected no next-server for VirtualMedia, got %s", resp.ServerIPAddr)
							}
						})
					}
				})
			}
		})
	}
}

func TestDHCPHandlerUnknownMAC(t *testing.T) {
	mac, _ := net.ParseMAC("ff:ff:ff:ff:ff:ff")
	serverIP := net.ParseIP("10.0.1.254").To4()

	reader := newFakeReader(t)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written != nil {
		t.Error("expected no response for unknown MAC")
	}
}

func TestDHCPHandlerRequest(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	// First get an offer
	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}
	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected offer")
	}

	offer, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	request, err := dhcpv4.NewRequestFromOffer(offer)
	if err != nil {
		t.Fatal(err)
	}

	conn = &fakePacketConn{}
	srv.handler(conn, peer, request)

	if conn.written == nil {
		t.Fatal("expected ACK response")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("expected Ack, got %s", resp.MessageType())
	}
}

func TestDHCPHandlerNoImageAllowsPXEDiscover(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate UEFI firmware PXE boot: add PXEClient vendor class identifier
	discover.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016"))

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}
	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}

	if resp.GetOneOption(dhcpv4.OptionTFTPServerName) != nil {
		t.Error("expected no TFTP server option when no image is configured")
	}

	if resp.GetOneOption(dhcpv4.OptionBootfileName) != nil {
		t.Error("expected no bootfile option when no image is configured")
	}
}

func TestDHCPHandlerNoImageAllowsHTTPBootDiscover(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate UEFI HTTP Boot: add HTTPClient vendor class identifier
	discover.UpdateOption(dhcpv4.OptClassIdentifier("HTTPClient:Arch:00016:UNDI:003016"))

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}
	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}

	if resp.GetOneOption(dhcpv4.OptionTFTPServerName) != nil {
		t.Error("expected no TFTP server option when no image is configured")
	}

	if resp.GetOneOption(dhcpv4.OptionBootfileName) != nil {
		t.Error("expected no bootfile option when no image is configured")
	}
}

func TestDHCPHandlerPXEDisabledAllowsNonPXEDiscover(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
	}

	// Normal DHCP discover without PXE vendor class (e.g. from OS DHCP client)
	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}
	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response for non-PXE discover when PXE is disabled")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}
}

func TestDHCPHandlerRelayOnlyRejectsDirectPacket(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		// Interface intentionally left empty: relay-only mode.
		Reader:   reader,
		ServerIP: serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	// No GatewayIPAddr set - this is a direct client packet, not from a relay.
	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written != nil {
		t.Error("expected no response for direct (non-relay) packet in relay-only mode")
	}
}

func TestDHCPHandlerRelayAgent(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f0",
				IPv4:       "10.0.1.10",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Reader:   reader,
		ServerIP: serverIP,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	discover.GatewayIPAddr = net.ParseIP("10.0.1.1").To4()

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response")
	}

	udpDest, ok := conn.dest.(*net.UDPAddr)
	if !ok {
		t.Fatal("expected UDP destination")
	}

	if !udpDest.IP.Equal(net.ParseIP("10.0.1.1")) {
		t.Errorf("expected response to relay 10.0.1.1, got %s", udpDest.IP)
	}

	if udpDest.Port != 67 {
		t.Errorf("expected port 67, got %d", udpDest.Port)
	}
}

func newFakeReader(t *testing.T, objs ...runtime.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	clientObjs := make([]client.Object, len(objs))
	for i, o := range objs {
		clientObjs[i] = o.(client.Object)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(clientObjs...).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByMAC, indexing.IndexNodeByMACFunc).
		Build()
}

type fakePacketConn struct {
	written []byte
	dest    net.Addr
}

func (f *fakePacketConn) ReadFrom(b []byte) (int, net.Addr, error) { return 0, nil, nil }
func (f *fakePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	f.written = make([]byte, len(b))
	copy(f.written, b)
	f.dest = addr

	return len(b), nil
}
func (f *fakePacketConn) Close() error                       { return nil }
func (f *fakePacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (f *fakePacketConn) SetDeadline(_ time.Time) error      { return nil }
func (f *fakePacketConn) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakePacketConn) SetWriteDeadline(_ time.Time) error { return nil }

// newTestImages pushes an image whose disk/metadata.yaml is metadata to an
// in-memory registry, tagged as ref for architecture arch (default amd64),
// and returns a FileResolver that pulls from that registry.
func newTestImages(t *testing.T, ref, arch, metadata string) *netboot.FileResolver {
	t.Helper()

	reg := imagepulltest.NewRegistry(t)

	parsed, err := registry.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}

	if arch == "" {
		arch = v1alpha3.DefaultPXEArchitecture
	}

	reg.PushMultiArch(t, parsed.Repository, parsed.Reference, map[string]map[string][]byte{
		arch: {"disk/metadata.yaml": []byte(metadata)},
	})

	return &netboot.FileResolver{
		Puller: &imagepull.Puller{HTTPClient: &http.Client{Transport: &redirectTransport{host: reg.Host()}}},
	}
}

// redirectTransport sends every request to host over plain HTTP, so tests
// can use realistic registry hosts.
type redirectTransport struct {
	host string
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	r.URL.Host = rt.host
	r.Host = rt.host

	resp, err := http.DefaultTransport.RoundTrip(r)
	if resp != nil {
		// Report the original request so that the registry client treats
		// the response as coming from the original host.
		resp.Request = req
	}

	return resp, err
}

func TestDHCPHandlerUnavailableImageOmitsBootFile(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f6")
	serverIP := net.ParseIP("10.0.1.254").To4()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-unavailable"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{NetbootImage: "ghcr.io/test/missing:v1", DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f6",
				IPv4:       "10.0.1.16",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	// The registry has no image for the reference.
	images := newTestImages(t, "ghcr.io/test/other:v1", "", "dhcpBootImageName: shimx64.efi\n")

	srv := &Server{Interface: "eth0", Reader: newFakeReader(t, node), ServerIP: serverIP, Images: images}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	srv.handler(conn, &net.UDPAddr{IP: net.ParseIP("10.0.1.16"), Port: 68}, discover)

	if conn.written == nil {
		t.Fatal("expected DHCP response, got none")
	}

	resp, err := dhcpv4.FromBytes(conn.written)
	if err != nil {
		t.Fatal(err)
	}

	if resp.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("expected Offer, got %s", resp.MessageType())
	}

	if bootfile := resp.BootFileNameOption(); bootfile != "" {
		t.Errorf("expected no bootfile while the image is unavailable, got %s", bootfile)
	}
}

// gatedTransport blocks every request until gate is closed.
type gatedTransport struct {
	gate <-chan struct{}
	next http.RoundTripper
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-g.gate
	return g.next.RoundTrip(req)
}

func TestDHCPHandlerMetadataTimeoutWarmsMemo(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f7")
	serverIP := net.ParseIP("10.0.1.254").To4()
	netbootImageRef := "ghcr.io/test/slow-netboot:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-slow"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{NetbootImage: netbootImageRef, DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f7",
				IPv4:       "10.0.1.17",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	images := newTestImages(t, netbootImageRef, "", "dhcpBootImageName: shimx64.efi\n")
	gate := make(chan struct{})
	images.Puller.HTTPClient.Transport = &gatedTransport{gate: gate, next: images.Puller.HTTPClient.Transport}
	images.Metadata = &netboot.MetadataMemo{}

	srv := &Server{
		Interface: "eth0", Reader: newFakeReader(t, node), ServerIP: serverIP, Images: images,
		MetadataTimeout: 20 * time.Millisecond,
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	bootFile := func() string {
		conn := &fakePacketConn{}
		srv.handler(conn, &net.UDPAddr{IP: net.ParseIP("10.0.1.17"), Port: 68}, discover)

		if conn.written == nil {
			t.Fatal("expected DHCP response, got none")
		}

		resp, err := dhcpv4.FromBytes(conn.written)
		if err != nil {
			t.Fatal(err)
		}

		return resp.BootFileNameOption()
	}

	// The registry is stalled: the reply goes out without a boot file.
	if got := bootFile(); got != "" {
		t.Fatalf("expected no bootfile while the registry is stalled, got %q", got)
	}

	// The lookup continues in the background, so a retransmit is answered
	// from the warmed memo.
	close(gate)

	deadline := time.Now().Add(5 * time.Second)
	for bootFile() != "shimx64.efi" {
		if time.Now().After(deadline) {
			t.Fatal("retransmit did not get the warmed boot file")
		}

		time.Sleep(10 * time.Millisecond)
	}
}
