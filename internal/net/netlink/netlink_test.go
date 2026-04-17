// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"net/netip"
	"testing"
)

// Note: Most netlink operations require root privileges and actual network interfaces.
// These tests focus on the logic that can be tested without root, or are marked as
// integration tests that can be skipped in CI environments.

func TestLinkManager_parseAddress(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantIP     string
		wantPrefix int
		wantErr    bool
	}{
		{
			name:       "IPv4 with prefix",
			input:      "10.0.0.1/24",
			wantIP:     "10.0.0.1",
			wantPrefix: 24,
		},
		{
			name:       "IPv4 without prefix",
			input:      "10.0.0.1",
			wantIP:     "10.0.0.1",
			wantPrefix: 32,
		},
		{
			name:       "IPv6 with prefix",
			input:      "fd00::1/64",
			wantIP:     "fd00::1",
			wantPrefix: 64,
		},
		{
			name:       "IPv6 without prefix",
			input:      "fd00::1",
			wantIP:     "fd00::1",
			wantPrefix: 128,
		},
		{
			name:    "invalid IP",
			input:   "not-an-ip",
			wantErr: true,
		},
		{
			name:    "invalid CIDR",
			input:   "10.0.0.1/abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, err := parseAddress(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				}

				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if addr.IP.String() != tt.wantIP {
				t.Errorf("expected IP %s, got %s", tt.wantIP, addr.IP.String())
			}

			ones, _ := addr.Mask.Size()
			if ones != tt.wantPrefix {
				t.Errorf("expected prefix %d, got %d", tt.wantPrefix, ones)
			}
		})
	}
}

// TestLinkManager_NewLinkManager tests link manager new link manager.
func TestLinkManager_NewLinkManager(t *testing.T) {
	lm := NewLinkManager("test-iface")
	if lm == nil {
		t.Fatal("expected non-nil LinkManager")
	}

	if lm.ifaceName != "test-iface" {
		t.Errorf("expected interface name 'test-iface', got '%s'", lm.ifaceName)
	}
}

// TestWireGuardMTUOverhead verifies the overhead constant.
func TestWireGuardMTUOverhead(t *testing.T) {
	if WireGuardMTUOverhead != 80 {
		t.Errorf("expected WireGuardMTUOverhead to be 80, got %d", WireGuardMTUOverhead)
	}
}

// TestGeneveMTUOverhead verifies the GENEVE overhead constant.
func TestGeneveMTUOverhead(t *testing.T) {
	if GeneveMTUOverhead != 58 {
		t.Errorf("expected GeneveMTUOverhead to be 58, got %d", GeneveMTUOverhead)
	}
}

// TestLinkManager_EnsureMTU_NonExistentInterface verifies EnsureMTU returns an
// error for a non-existent interface.
func TestLinkManager_EnsureMTU_NonExistentInterface(t *testing.T) {
	lm := NewLinkManager("does-not-exist-test-iface")

	err := lm.EnsureMTU(1420)
	if err == nil {
		t.Error("expected error for non-existent interface, got nil")
	}
}

// TestLinkManager_Integration tests link manager integration.
func TestLinkManager_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This would test actual interface creation/deletion
	// Skip unless running as root
	t.Skip("integration test requires root privileges")
}

// TestWireGuardManager_prefixToIPNet tests wire guard manager prefix to ipnet.
func TestWireGuardManager_prefixToIPNet(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantIP     string
		wantPrefix int
	}{
		{
			name:       "IPv4 /24",
			input:      "10.0.0.0/24",
			wantIP:     "10.0.0.0",
			wantPrefix: 24,
		},
		{
			name:       "IPv4 /32",
			input:      "10.0.0.1/32",
			wantIP:     "10.0.0.1",
			wantPrefix: 32,
		},
		{
			name:       "IPv6 /64",
			input:      "fd00::/64",
			wantIP:     "fd00::",
			wantPrefix: 64,
		},
		{
			name:       "IPv6 /128",
			input:      "fd00::1/128",
			wantIP:     "fd00::1",
			wantPrefix: 128,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, err := netip.ParsePrefix(tt.input)
			if err != nil {
				t.Fatalf("failed to parse prefix: %v", err)
			}

			ipNet := prefixToIPNet(prefix)

			if ipNet.IP.String() != tt.wantIP {
				t.Errorf("expected IP %s, got %s", tt.wantIP, ipNet.IP.String())
			}

			ones, _ := ipNet.Mask.Size()
			if ones != tt.wantPrefix {
				t.Errorf("expected prefix %d, got %d", tt.wantPrefix, ones)
			}
		})
	}
}

// TestWireGuardPeer_AllowedIPsParsing tests wire guard peer allowed ips parsing.
func TestWireGuardPeer_AllowedIPsParsing(t *testing.T) {
	// Test that we correctly handle various allowed IP formats
	tests := []struct {
		name       string
		allowedIPs []string
		wantCount  int
	}{
		{
			name:       "IPv4 CIDRs",
			allowedIPs: []string{"10.0.0.0/24", "192.168.1.0/24"},
			wantCount:  2,
		},
		{
			name:       "IPv6 CIDRs",
			allowedIPs: []string{"fd00::/64", "2001:db8::/32"},
			wantCount:  2,
		},
		{
			name:       "Mixed IPv4 and IPv6",
			allowedIPs: []string{"10.0.0.0/24", "fd00::/64"},
			wantCount:  2,
		},
		{
			name:       "Plain IP addresses (should get /32 or /128)",
			allowedIPs: []string{"10.0.0.1", "fd00::1"},
			wantCount:  2,
		},
		{
			name:       "Empty list",
			allowedIPs: []string{},
			wantCount:  0,
		},
		{
			name:       "With invalid entries",
			allowedIPs: []string{"10.0.0.0/24", "invalid", "192.168.1.0/24"},
			wantCount:  2, // invalid should be skipped
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// We can't fully test buildPeerConfig without a real WireGuard manager,
			// but we can test the allowed IPs parsing logic
			count := 0

			for _, cidr := range tt.allowedIPs {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil {
					// Try parsing as just an IP address
					addr, err := netip.ParseAddr(cidr)
					if err != nil {
						continue // Invalid, skip
					}
					// Convert to prefix
					if addr.Is4() {
						prefix = netip.PrefixFrom(addr, 32)
					} else {
						prefix = netip.PrefixFrom(addr, 128)
					}
				}

				_ = prefixToIPNet(prefix)
				count++
			}

			if count != tt.wantCount {
				t.Errorf("expected %d valid IPs, got %d", tt.wantCount, count)
			}
		})
	}
}
