// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestServePXEMediaTFTPServerFlag(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "default"},
		{name: "explicit", args: []string{"--media-tftp-server=10.0.1.254"}, want: "10.0.1.254"},
		{name: "separate argument", args: []string{"--media-tftp-server", "10.0.1.253"}, want: "10.0.1.253"},
		{name: "missing argument", args: []string{"--media-tftp-server"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ServePXECmd()

			flag := cmd.Flags().Lookup("media-tftp-server")
			if flag == nil {
				t.Fatal("serve-pxe must register --media-tftp-server")
			}

			if flag.DefValue != "" || flag.Value.Type() != "string" {
				t.Fatalf("expected string flag with empty default for server IP fallback, got %s default %q", flag.Value.Type(), flag.DefValue)
			}

			if err := cmd.ParseFlags(tt.args); (err != nil) != tt.wantErr {
				t.Fatalf("ParseFlags() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			got, err := cmd.Flags().GetString("media-tftp-server")
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("media-tftp-server = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServePXEExperimentalContainerIsolationFlag(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "off by default"},
		{name: "enabled", args: []string{"--experimental-container-isolation"}, want: true},
		{name: "explicitly off", args: []string{"--experimental-container-isolation=false"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ServePXECmd()

			flag := cmd.Flags().Lookup("experimental-container-isolation")
			if flag == nil {
				t.Fatal("serve-pxe must register --experimental-container-isolation")
			}

			if flag.DefValue != "false" || flag.Value.Type() != "bool" {
				t.Fatalf("expected bool flag defaulting to false, got %s default %q", flag.Value.Type(), flag.DefValue)
			}

			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}

			got, err := cmd.Flags().GetBool("experimental-container-isolation")
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("experimental-container-isolation = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestResolveServerAddress(t *testing.T) {
	detected := net.ParseIP("10.0.2.5")
	detector := func() (net.IP, error) { return detected, nil }
	noDetection := func() (net.IP, error) {
		return nil, errors.New("outbound detection must not run for an explicit bind address")
	}

	for _, tt := range []struct {
		name     string
		bind     string
		serveURL string
		outbound func() (net.IP, error)
		wantIP   string
		wantURL  string
		wantErr  bool
	}{
		{name: "unspecified bind with serve-url", bind: "0.0.0.0", serveURL: "http://pxe.example.com", outbound: detector, wantIP: "10.0.2.5", wantURL: "http://pxe.example.com"},
		{name: "empty bind with serve-url", bind: "", serveURL: "https://pxe.example.com", outbound: detector, wantIP: "10.0.2.5", wantURL: "https://pxe.example.com"},
		{name: "unspecified bind derives serve-url", bind: "0.0.0.0", outbound: detector, wantIP: "10.0.2.5", wantURL: "http://10.0.2.5:8880"},
		{name: "explicit bind", bind: "192.0.2.9", outbound: noDetection, wantIP: "192.0.2.9", wantURL: "http://192.0.2.9:8880"},
		{name: "explicit bind with serve-url", bind: "192.0.2.9", serveURL: "http://pxe.example.com", outbound: noDetection, wantIP: "192.0.2.9", wantURL: "http://pxe.example.com"},
		{name: "detector error", bind: "0.0.0.0", outbound: func() (net.IP, error) { return nil, errors.New("no route") }, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ip, url, err := resolveServerAddress(tt.bind, tt.serveURL, 8880, tt.outbound)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveServerAddress() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			if ip == nil || ip.IsUnspecified() {
				t.Errorf("resolveServerAddress() IP = %v, must not be unspecified", ip)
			}

			if got := ip.String(); got != tt.wantIP {
				t.Errorf("resolveServerAddress() IP = %q, want %q", got, tt.wantIP)
			}

			if url != tt.wantURL {
				t.Errorf("resolveServerAddress() serve URL = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

// A serve URL plus the default bind address must still advertise the
// provisioning interface address, and an explicit media TFTP server wins over
// it.
func TestResolveProvisioningMediaTFTPServer(t *testing.T) {
	_, _, err := resolveServerAddress("0.0.0.0", "http://pxe.example.com", 8880, func() (net.IP, error) {
		return net.ParseIP("10.0.2.5"), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	provisioningIP := net.ParseIP("10.0.3.7")

	got, err := resolveMediaTFTPServer("", provisioningIP)
	if err != nil {
		t.Fatal(err)
	}

	if got != "10.0.3.7" {
		t.Errorf("resolveMediaTFTPServer() = %q, want %q", got, "10.0.3.7")
	}

	got, err = resolveMediaTFTPServer("10.0.9.9", provisioningIP)
	if err != nil {
		t.Fatal(err)
	}

	if got != "10.0.9.9" {
		t.Errorf("resolveMediaTFTPServer() = %q, want %q", got, "10.0.9.9")
	}
}

func TestResolveMediaTFTPServer(t *testing.T) {
	for _, tt := range []struct {
		name     string
		raw      string
		serverIP net.IP
		want     string
		wantErr  bool
	}{
		{name: "explicit IPv4", raw: "192.0.2.2", want: "192.0.2.2"},
		{name: "explicit IPv4-mapped IPv6", raw: "::ffff:192.0.2.2", want: "192.0.2.2"},
		{name: "explicit overrides server IP", raw: "192.0.2.2", serverIP: net.ParseIP("192.0.2.1"), want: "192.0.2.2"},
		{name: "explicit with unspecified bind", raw: "192.0.2.2", serverIP: net.IPv4zero, want: "192.0.2.2"},
		{name: "default IPv4", serverIP: net.ParseIP("192.0.2.1"), want: "192.0.2.1"},
		{name: "default IPv4-mapped IPv6", serverIP: net.ParseIP("::ffff:192.0.2.1"), want: "192.0.2.1"},
		{name: "default four-byte IPv4", serverIP: net.ParseIP("192.0.2.1").To4(), want: "192.0.2.1"},
		{name: "default unspecified IPv4", serverIP: net.IPv4zero},
		{name: "default nil"},
		{name: "default IPv6", serverIP: net.ParseIP("2001:db8::1")},
		{name: "default unspecified IPv6", serverIP: net.IPv6zero},
		{name: "invalid explicit does not fall back", raw: "invalid", serverIP: net.ParseIP("192.0.2.1"), wantErr: true},
		{name: "explicit unspecified IPv4", raw: "0.0.0.0", wantErr: true},
		{name: "explicit unspecified IPv6", raw: "::", wantErr: true},
		{name: "explicit IPv6", raw: "2001:db8::1", wantErr: true},
		{name: "explicit hostname", raw: "tftp.example.com", wantErr: true},
		{name: "explicit URL", raw: "tftp://192.0.2.2", wantErr: true},
		{name: "explicit port", raw: "192.0.2.2:69", wantErr: true},
		{name: "explicit whitespace", raw: " ", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMediaTFTPServer(tt.raw, tt.serverIP)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveMediaTFTPServer() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && !strings.Contains(err.Error(), "--media-tftp-server") {
				t.Errorf("expected error to identify --media-tftp-server, got %v", err)
			}

			if got != tt.want {
				t.Errorf("resolveMediaTFTPServer() = %q, want %q", got, tt.want)
			}
		})
	}
}
