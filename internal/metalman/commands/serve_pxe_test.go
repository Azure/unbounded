// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"errors"
	"net"
	"testing"
)

func TestResolveAdvertisedIP(t *testing.T) {
	t.Parallel()

	outboundErr := errors.New("no outbound")
	outbound := func(ip net.IP, err error) func() (net.IP, error) {
		return func() (net.IP, error) { return ip, err }
	}

	tests := []struct {
		name        string
		bindAddress string
		advertiseIP string
		outbound    func() (net.IP, error)
		want        string
		wantErr     bool
	}{
		{
			name:        "advertise overrides bind",
			bindAddress: "127.0.0.1",
			advertiseIP: "172.31.99.2",
			outbound:    outbound(nil, outboundErr),
			want:        "172.31.99.2",
		},
		{
			name:        "advertise trims whitespace",
			bindAddress: "127.0.0.1",
			advertiseIP: "  172.31.99.2  ",
			outbound:    outbound(nil, outboundErr),
			want:        "172.31.99.2",
		},
		{
			name:        "defaults to bind address when advertise empty",
			bindAddress: "10.0.0.5",
			advertiseIP: "",
			outbound:    outbound(nil, outboundErr),
			want:        "10.0.0.5",
		},
		{
			name:        "unspecified bind resolves via outbound",
			bindAddress: "0.0.0.0",
			advertiseIP: "",
			outbound:    outbound(net.ParseIP("192.168.1.20"), nil),
			want:        "192.168.1.20",
		},
		{
			name:        "advertise takes precedence over unspecified bind",
			bindAddress: "0.0.0.0",
			advertiseIP: "172.31.99.2",
			outbound:    outbound(nil, outboundErr),
			want:        "172.31.99.2",
		},
		{
			name:        "invalid advertise ip errors",
			bindAddress: "127.0.0.1",
			advertiseIP: "not-an-ip",
			outbound:    outbound(nil, outboundErr),
			wantErr:     true,
		},
		{
			name:        "invalid bind ip errors",
			bindAddress: "not-an-ip",
			advertiseIP: "",
			outbound:    outbound(nil, outboundErr),
			wantErr:     true,
		},
		{
			name:        "outbound failure propagates",
			bindAddress: "0.0.0.0",
			advertiseIP: "",
			outbound:    outbound(nil, outboundErr),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveAdvertisedIP(tt.bindAddress, tt.advertiseIP, tt.outbound)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %v)", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.String() != tt.want {
				t.Fatalf("resolveAdvertisedIP = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestResolveDHCPInterfaceIP(t *testing.T) {
	t.Parallel()

	t.Run("uses concrete server IP", func(t *testing.T) {
		t.Parallel()

		got, err := resolveDHCPInterfaceIP(net.ParseIP("10.0.0.5"), func() (net.IP, error) {
			return nil, errors.New("unexpected outbound lookup")
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got.String() != "10.0.0.5" {
			t.Fatalf("resolveDHCPInterfaceIP = %q, want %q", got.String(), "10.0.0.5")
		}
	})

	t.Run("resolves unspecified server IP", func(t *testing.T) {
		t.Parallel()

		got, err := resolveDHCPInterfaceIP(net.ParseIP("0.0.0.0"), func() (net.IP, error) {
			return net.ParseIP("192.168.1.20"), nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got.String() != "192.168.1.20" {
			t.Fatalf("resolveDHCPInterfaceIP = %q, want %q", got.String(), "192.168.1.20")
		}
	})
}
