// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
	"github.com/Azure/unbounded-kube/internal/metalman/netboot"
)

// BootstrapConfig holds settings for automatic Machine creation when a
// DHCP request arrives from an unknown MAC address. When nil on the
// Server, bootstrap mode is disabled and unknown MACs are silently
// ignored (the existing behavior).
type BootstrapConfig struct {
	// Client is a read-write Kubernetes client used to create Machine
	// objects. It must be non-nil.
	Client client.Client

	// APIReader is an uncached reader used for the MAC double-check
	// before creating a Machine. Using a direct API read avoids the
	// informer cache propagation delay that could miss recently
	// created Machines.
	APIReader client.Reader

	// Image is the OCI image reference to set on bootstrapped Machines
	// (e.g. "ghcr.io/azure/images/host-ubuntu2404:v1").
	Image string

	// Site is the optional site label value. When non-empty, created
	// Machines are labeled with unbounded-kube.io/site=<value>.
	Site string
}

type Server struct {
	Interface string
	Port      int
	Reader    client.Reader
	ServerIP  net.IP
	OCICache  *netboot.OCICache

	// Bootstrap, when non-nil, enables automatic Machine creation for
	// unknown MAC addresses. See BootstrapConfig for details.
	Bootstrap *BootstrapConfig

	// bootstrapInFlight tracks MAC addresses for which a bootstrap
	// Machine creation is currently in progress, preventing duplicate
	// concurrent API calls from rapid DHCP retries.
	bootstrapInFlight sync.Map
}

func (s *Server) NeedLeaderElection() bool {
	// Responding to unicast packets is always safe.
	// But we need election when binding to an interface (e.g. listening for multicast)
	return s.Interface != ""
}

func (s *Server) Start(ctx context.Context) error {
	port := s.Port
	if port == 0 {
		port = 67
	}

	// Bind to an interface if configured
	var conn net.PacketConn

	if s.Interface != "" {
		srv, err := server4.NewServer(s.Interface, &net.UDPAddr{IP: net.IPv4zero, Port: port}, s.handler)
		if err != nil {
			return fmt.Errorf("creating DHCP server: %w", err)
		}

		go func() {
			<-ctx.Done()
			srv.Close() //nolint:errcheck // Best-effort shutdown of DHCP server.
		}()

		slog.Info("starting DHCP server", "interface", s.Interface, "port", port)

		return srv.Serve()
	}

	// Bind to a port if not bound to an interface
	var err error

	conn, err = net.ListenPacket("udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("creating DHCP listener: %w", err)
	}

	go func() {
		<-ctx.Done()
		conn.Close() //nolint:errcheck // Best-effort shutdown of DHCP listener.
	}()

	slog.Info("starting DHCP server (relay only)", "port", port)
	s.serve(conn)

	return nil
}

func (s *Server) serve(conn net.PacketConn) {
	defer conn.Close() //nolint:errcheck // Best-effort close of DHCP connection.

	buf := make([]byte, 4096)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			slog.Error("reading DHCP packet", "err", err)
			time.Sleep(50 * time.Millisecond)

			continue
		}

		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			slog.Error("parsing DHCP packet", "err", err)
			continue
		}

		go s.handler(conn, peer, m)
	}
}

func (s *Server) handler(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	if m.MessageType() != dhcpv4.MessageTypeDiscover && m.MessageType() != dhcpv4.MessageTypeRequest {
		return
	}

	// In relay-only mode (no interface configured), only respond to packets
	// forwarded by a DHCP relay agent. Relay agents set the GatewayIPAddr
	// (giaddr) field; direct client packets leave it zeroed.
	if s.Interface == "" && (m.GatewayIPAddr == nil || m.GatewayIPAddr.IsUnspecified()) {
		return
	}

	mac := strings.ToLower(m.ClientHWAddr.String())
	log := slog.With("mac", mac, "type", m.MessageType().String())

	ctx := context.Background()

	var list v1alpha3.MachineList
	if err := s.Reader.List(ctx, &list, client.MatchingFields{indexing.IndexNodeByMAC: mac}); err != nil {
		log.Error("listing Machines by MAC", "err", err)
		return
	}

	if len(list.Items) == 0 {
		if s.Bootstrap != nil && mac != "" {
			s.bootstrapMachine(ctx, log, mac)
		}

		return
	}

	node := &list.Items[0]
	if node.Spec.PXE == nil {
		return
	}

	var lease *v1alpha3.DHCPLease

	for i := range node.Spec.PXE.DHCPLeases {
		if strings.EqualFold(node.Spec.PXE.DHCPLeases[i].MAC, mac) {
			lease = &node.Spec.PXE.DHCPLeases[i]
			break
		}
	}

	if lease == nil {
		return
	}

	clientIP := net.ParseIP(lease.IPv4).To4()
	if clientIP == nil {
		log.Error("invalid IPv4 on lease", "ipv4", lease.IPv4)
		return
	}

	resp, err := dhcpv4.NewReplyFromRequest(m,
		dhcpv4.WithYourIP(clientIP),
		dhcpv4.WithServerIP(s.ServerIP),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.IPMask(net.ParseIP(lease.SubnetMask).To4()))),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.ServerIP)),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(86400*time.Second)),
	)
	if err != nil {
		log.Error("building DHCP response", "err", err)
		return
	}

	if gw := net.ParseIP(lease.Gateway).To4(); gw != nil {
		resp.UpdateOption(dhcpv4.OptRouter(gw))
	}

	if len(lease.DNS) > 0 {
		dnsIPs := make([]net.IP, 0, len(lease.DNS))
		for _, s := range lease.DNS {
			if ip := net.ParseIP(s).To4(); ip != nil {
				dnsIPs = append(dnsIPs, ip)
			}
		}

		if len(dnsIPs) > 0 {
			resp.UpdateOption(dhcpv4.OptDNS(dnsIPs...))
		}
	}

	if node.Spec.PXE.Image != "" && s.OCICache != nil {
		meta, err := s.OCICache.MetadataForRef(node.Spec.PXE.Image)
		if err != nil {
			log.Warn("OCI image metadata not available", "image", node.Spec.PXE.Image, "err", err)
		} else if meta.DHCPBootImageName != "" {
			resp.UpdateOption(dhcpv4.OptTFTPServerName(s.ServerIP.String()))
			resp.UpdateOption(dhcpv4.OptBootFileName(meta.DHCPBootImageName))
			resp.ServerIPAddr = s.ServerIP
		}
	}

	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
	case dhcpv4.MessageTypeRequest:
		resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
	}

	var dest net.Addr
	if !m.GatewayIPAddr.IsUnspecified() && m.GatewayIPAddr != nil {
		dest = &net.UDPAddr{IP: m.GatewayIPAddr, Port: 67}
	} else {
		dest = peer
	}

	log.Info("sending DHCP response", "node", node.Name, "ip", lease.IPv4, "response", resp.MessageType().String())

	if _, err := conn.WriteTo(resp.ToBytes(), dest); err != nil {
		log.Error("sending DHCP response", "err", err)
	}
}

const siteLabel = "unbounded-kube.io/site"

// bootstrapMachine creates a Machine object for an unknown MAC address.
// It uses generateName for random naming and guards against duplicate
// creation with both an in-memory dedup map and a server-side MAC index
// check. Concurrent calls for the same MAC are coalesced via
// bootstrapInFlight.
func (s *Server) bootstrapMachine(ctx context.Context, log *slog.Logger, mac string) {
	// Fast-path: if another goroutine is already creating a Machine for
	// this MAC, skip. LoadOrStore returns true if the key was already
	// present.
	if _, loaded := s.bootstrapInFlight.LoadOrStore(mac, struct{}{}); loaded {
		return
	}
	defer s.bootstrapInFlight.Delete(mac)

	// Double-check the MAC index in case a Machine was created between
	// the caller's List and this point (e.g. by a concurrent handler
	// that just finished, or an external actor).
	var existing v1alpha3.MachineList
	if err := s.Bootstrap.APIReader.List(ctx, &existing); err != nil {
		log.Error("bootstrap: re-checking MAC via API", "err", err)
		return
	}

	for i := range existing.Items {
		if existing.Items[i].Spec.PXE == nil {
			continue
		}
		for _, lease := range existing.Items[i].Spec.PXE.DHCPLeases {
			if strings.EqualFold(lease.MAC, mac) {
				log.Info("bootstrap: Machine already exists for MAC", "machine", existing.Items[i].Name)
				return
			}
		}
	}

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "machine-",
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: s.Bootstrap.Image,
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: mac,
				}},
			},
		},
	}

	if s.Bootstrap.Site != "" {
		machine.Labels = map[string]string{
			siteLabel: s.Bootstrap.Site,
		}
	}

	if err := s.Bootstrap.Client.Create(ctx, machine); err != nil {
		// With generateName, AlreadyExists should not normally occur since
		// names are generated server-side. This guard handles any unexpected
		// API-level conflict gracefully.
		if apierrors.IsAlreadyExists(err) {
			log.Info("bootstrap: Machine creation conflict (already exists)")
			return
		}

		log.Error("bootstrap: creating Machine", "err", err)

		return
	}

	log.Info("bootstrap: created Machine for unknown MAC", "machine", machine.Name)
}
