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
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

type Server struct {
	Interface         string
	Port              int
	Reader            client.Reader
	DecisionProvider  DecisionProvider
	ServerIP          net.IP
	OCICache          *netboot.OCICache
	ServeURL          string
	DefaultNetbootRef string
}

// Decision is the immutable lease and boot information returned by the
// Metalman server for one ready netboot session.
type Decision struct {
	Lease     v1alpha3.DHCPLease        `json:"lease"`
	Transport v1alpha3.NetbootTransport `json:"transport"`
	BootFile  string                    `json:"bootFile"`
}

// DecisionProvider resolves the active ready session for a DHCP client.
type DecisionProvider interface {
	Decide(ctx context.Context, mac string, httpClient bool) (*Decision, error)
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

	decision, node, err := s.decision(ctx, mac, isHTTPClientRequest(m))
	if err != nil {
		log.Error("resolving DHCP decision", "err", err)
		return
	}
	if decision == nil {
		return
	}
	lease := &decision.Lease

	clientIP := net.ParseIP(lease.IPv4).To4()
	if clientIP == nil {
		log.Error("invalid IPv4 on lease", "ipv4", lease.IPv4)
		return
	}

	resp, err := dhcpv4.NewReplyFromRequest(
		m,
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

	if s.DecisionProvider != nil {
		switch decision.Transport {
		case v1alpha3.NetbootTransportHTTP:
			if isHTTPClientRequest(m) && decision.BootFile != "" {
				resp.UpdateOption(dhcpv4.OptBootFileName(decision.BootFile))
			}
		case v1alpha3.NetbootTransportTFTP:
			if decision.BootFile != "" {
				resp.UpdateOption(dhcpv4.OptTFTPServerName(s.ServerIP.String()))
				resp.UpdateOption(dhcpv4.OptBootFileName(decision.BootFile))
				resp.ServerIPAddr = s.ServerIP
			}
		}
	} else if node != nil {
		netbootImage := node.Spec.Netboot().NetbootImage
		if netbootImage == "" {
			netbootImage = s.DefaultNetbootRef
		}

		if netbootImage == "" || s.OCICache == nil {
			goto send
		}
		architecture := node.Spec.Netboot().TargetArchitecture()

		meta, err := s.OCICache.MetadataForRefArchitecture(netbootImage, architecture)
		if err != nil {
			log.Warn("OCI image metadata not available", "image", netbootImage, "architecture", architecture, "err", err)
		} else if node.Spec.Netboot().TargetTransport() == v1alpha3.NetbootTransportHTTP {
			if isHTTPClientRequest(m) {
				bootURL, err := netboot.JoinServeURLPath(s.ServeURL, netboot.HTTPBootPathFromMetadata(meta))
				if err != nil {
					log.Warn("HTTP boot URL not available", "image", netbootImage, "architecture", architecture, "err", err)
				} else {
					resp.UpdateOption(dhcpv4.OptBootFileName(bootURL))
				}
			}
		} else if meta.DHCPBootImageName != "" {
			resp.UpdateOption(dhcpv4.OptTFTPServerName(s.ServerIP.String()))
			resp.UpdateOption(dhcpv4.OptBootFileName(meta.DHCPBootImageName))
			resp.ServerIPAddr = s.ServerIP
		}
	}

send:
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

	log.Info("sending DHCP response", "ip", lease.IPv4, "response", resp.MessageType().String())

	if _, err := conn.WriteTo(resp.ToBytes(), dest); err != nil {
		log.Error("sending DHCP response", "err", err)
	}
}

func (s *Server) decision(ctx context.Context, mac string, httpClient bool) (*Decision, *v1alpha3.Machine, error) {
	if s.DecisionProvider != nil {
		decision, err := s.DecisionProvider.Decide(ctx, mac, httpClient)
		return decision, nil, err
	}
	if s.Reader == nil {
		return nil, nil, errors.New("DHCP decision provider is not configured")
	}

	var list v1alpha3.MachineList
	if err := s.Reader.List(ctx, &list, client.MatchingFields{indexing.IndexNodeByMAC: mac}); err != nil {
		return nil, nil, fmt.Errorf("listing Machines by MAC: %w", err)
	}
	if len(list.Items) == 0 || list.Items[0].Spec.Netboot() == nil {
		return nil, nil, nil
	}

	node := &list.Items[0]
	for i := range node.Spec.Netboot().DHCPLeases {
		lease := &node.Spec.Netboot().DHCPLeases[i]
		if strings.EqualFold(lease.MAC, mac) {
			return &Decision{Lease: *lease}, node, nil
		}
	}

	return nil, nil, nil
}

func isHTTPClientRequest(m *dhcpv4.DHCPv4) bool {
	return strings.HasPrefix(m.ClassIdentifier(), "HTTPClient")
}
