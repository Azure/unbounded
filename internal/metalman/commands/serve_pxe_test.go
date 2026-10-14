// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
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
