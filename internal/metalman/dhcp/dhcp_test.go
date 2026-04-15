// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dhcp

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
	"github.com/Azure/unbounded-kube/internal/metalman/netboot"
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

	imageRef := "ghcr.io/test/ubuntu-24-04:v1"

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-02"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: imageRef, DHCPLeases: []v1alpha3.DHCPLease{{
				MAC:        "aa:bb:cc:dd:ee:f1",
				IPv4:       "10.0.1.11",
				SubnetMask: "255.255.255.0",
			}}},
		},
	}

	// Set up OCI cache with metadata for the image ref.
	cacheDir := t.TempDir()
	ociCache := netboot.NewOCICache(cacheDir)

	digest := "sha256:abcdef1234567890"
	ociCache.SetDigest(imageRef, digest)

	diskDir := filepath.Join(ociCache.DiskDir(digest))
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(diskDir, "metadata.yaml"),
		[]byte("dhcpBootImageName: shimx64.efi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader := newFakeReader(t, node)
	srv := &Server{
		Interface: "eth0",
		Reader:    reader,
		ServerIP:  serverIP,
		OCICache:  ociCache,
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

func newFakeClient(t *testing.T, objs ...runtime.Object) client.Client {
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

func TestDHCPHandlerBootstrapCreatesMachine(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:99")
	serverIP := net.ParseIP("10.0.1.254").To4()

	fc := newFakeClient(t)
	srv := &Server{
		Interface: "eth0",
		Reader:    fc,
		ServerIP:  serverIP,
		Bootstrap: &BootstrapConfig{
			Client:    fc,
			APIReader: fc,
			Image:     "ghcr.io/test/bootstrap:v1",
			Site:      "rack-a",
		},
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

	srv.handler(conn, peer, discover)

	// No DHCP response should be sent (no IP allocated yet).
	if conn.written != nil {
		t.Error("expected no DHCP response for bootstrapped MAC (IP not yet allocated)")
	}

	// Verify a Machine was created.
	var list v1alpha3.MachineList
	if err := fc.List(t.Context(), &list); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}

	if len(list.Items) != 1 {
		t.Fatalf("expected 1 Machine, got %d", len(list.Items))
	}

	machine := list.Items[0]

	// Verify generateName was used.
	if machine.GenerateName != "machine-" {
		t.Errorf("expected generateName 'machine-', got %q", machine.GenerateName)
	}

	// Verify PXE image.
	if machine.Spec.PXE == nil || machine.Spec.PXE.Image != "ghcr.io/test/bootstrap:v1" {
		t.Errorf("expected PXE image 'ghcr.io/test/bootstrap:v1', got %v", machine.Spec.PXE)
	}

	// Verify DHCP lease MAC.
	if len(machine.Spec.PXE.DHCPLeases) != 1 || machine.Spec.PXE.DHCPLeases[0].MAC != "aa:bb:cc:dd:ee:99" {
		t.Errorf("expected DHCP lease with MAC aa:bb:cc:dd:ee:99, got %v", machine.Spec.PXE.DHCPLeases)
	}

	// Verify site label.
	if machine.Labels["unbounded-kube.io/site"] != "rack-a" {
		t.Errorf("expected site label 'rack-a', got %q", machine.Labels["unbounded-kube.io/site"])
	}

	// Verify no IPv4 was set (IP allocator handles that).
	if machine.Spec.PXE.DHCPLeases[0].IPv4 != "" {
		t.Errorf("expected empty IPv4 (for IP allocator), got %q", machine.Spec.PXE.DHCPLeases[0].IPv4)
	}
}

func TestDHCPHandlerBootstrapNoSiteLabel(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:98")
	serverIP := net.ParseIP("10.0.1.254").To4()

	fc := newFakeClient(t)
	srv := &Server{
		Interface: "eth0",
		Reader:    fc,
		ServerIP:  serverIP,
		Bootstrap: &BootstrapConfig{
			Client:    fc,
			APIReader: fc,
			Image:     "ghcr.io/test/bootstrap:v1",
			// Site intentionally left empty.
		},
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

	srv.handler(conn, peer, discover)

	var list v1alpha3.MachineList
	if err := fc.List(t.Context(), &list); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}

	if len(list.Items) != 1 {
		t.Fatalf("expected 1 Machine, got %d", len(list.Items))
	}

	// Verify no site label.
	if _, ok := list.Items[0].Labels["unbounded-kube.io/site"]; ok {
		t.Error("expected no site label when Site is empty")
	}
}

func TestDHCPHandlerBootstrapSkipsExistingMAC(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:f0")
	serverIP := net.ParseIP("10.0.1.254").To4()

	// Pre-existing Machine with this MAC.
	existingNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:f0",
					IPv4:       "10.0.1.10",
					SubnetMask: "255.255.255.0",
				}},
			},
		},
	}

	fc := newFakeClient(t, existingNode)
	srv := &Server{
		Interface: "eth0",
		Reader:    fc,
		ServerIP:  serverIP,
		Bootstrap: &BootstrapConfig{
			Client:    fc,
			APIReader: fc,
			Image:     "ghcr.io/test/bootstrap:v1",
		},
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.10"), Port: 68}

	srv.handler(conn, peer, discover)

	// Should have responded normally (existing Machine has an IP).
	if conn.written == nil {
		t.Fatal("expected DHCP response for existing Machine")
	}

	// Verify no new Machine was created.
	var list v1alpha3.MachineList
	if err := fc.List(t.Context(), &list); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}

	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 Machine (the existing one), got %d", len(list.Items))
	}
}

func TestDHCPHandlerBootstrapDisabledIgnoresUnknownMAC(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:97")
	serverIP := net.ParseIP("10.0.1.254").To4()

	fc := newFakeClient(t)
	srv := &Server{
		Interface: "eth0",
		Reader:    fc,
		ServerIP:  serverIP,
		// Bootstrap intentionally nil (disabled).
	}

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}

	conn := &fakePacketConn{}
	peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

	srv.handler(conn, peer, discover)

	if conn.written != nil {
		t.Error("expected no response for unknown MAC without bootstrap")
	}

	// Verify no Machine was created.
	var list v1alpha3.MachineList
	if err := fc.List(t.Context(), &list); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}

	if len(list.Items) != 0 {
		t.Errorf("expected 0 Machines, got %d", len(list.Items))
	}
}

func TestDHCPHandlerBootstrapDeduplicatesConcurrent(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:96")
	serverIP := net.ParseIP("10.0.1.254").To4()

	fc := newFakeClient(t)
	srv := &Server{
		Interface: "eth0",
		Reader:    fc,
		ServerIP:  serverIP,
		Bootstrap: &BootstrapConfig{
			Client:    fc,
			APIReader: fc,
			Image:     "ghcr.io/test/bootstrap:v1",
		},
	}

	// Send multiple DHCP discovers for the same MAC.
	for i := 0; i < 5; i++ {
		discover, err := dhcpv4.NewDiscovery(mac)
		if err != nil {
			t.Fatal(err)
		}

		conn := &fakePacketConn{}
		peer := &net.UDPAddr{IP: net.ParseIP("10.0.1.99"), Port: 68}

		srv.handler(conn, peer, discover)
	}

	// Only one Machine should have been created.
	var list v1alpha3.MachineList
	if err := fc.List(t.Context(), &list); err != nil {
		t.Fatalf("listing Machines: %v", err)
	}

	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 Machine after multiple requests, got %d", len(list.Items))
	}
}
