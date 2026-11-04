// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
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

func TestServePXEPullFlags(t *testing.T) {
	cmd := ServePXECmd()

	for name, want := range map[string]string{
		"registry-mirror":  "",
		"pull-concurrency": strconv.Itoa(imagepull.DefaultConcurrency),
		"pull-chunk-size":  strconv.FormatInt(imagepull.DefaultChunkSize, 10),
	} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("serve-pxe must register --%s", name)
		}

		if flag.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, flag.DefValue, want)
		}
	}

	if err := cmd.ParseFlags([]string{"--registry-mirror=http://127.0.0.1:5000", "--pull-concurrency=2", "--pull-chunk-size=1048576"}); err != nil {
		t.Fatal(err)
	}

	if got, _ := cmd.Flags().GetInt64("pull-chunk-size"); got != 1<<20 {
		t.Errorf("pull-chunk-size = %d", got)
	}
}

func TestValidatePullFlags(t *testing.T) {
	for _, tt := range []struct {
		name        string
		concurrency int
		chunkSize   int64
		wantErr     bool
	}{
		{name: "defaults", concurrency: imagepull.DefaultConcurrency, chunkSize: imagepull.DefaultChunkSize},
		{name: "zero means default", concurrency: 0, chunkSize: 0},
		{name: "minimum chunk", concurrency: 1, chunkSize: 64 << 10},
		{name: "negative concurrency", concurrency: -1, chunkSize: imagepull.DefaultChunkSize, wantErr: true},
		{name: "negative chunk", concurrency: 1, chunkSize: -1, wantErr: true},
		{name: "chunk below minimum", concurrency: 1, chunkSize: 64<<10 - 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePullFlags(tt.concurrency, tt.chunkSize)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePullFlags(%d, %d) error = %v, wantErr %v", tt.concurrency, tt.chunkSize, err, tt.wantErr)
			}
		})
	}
}

func TestParseRegistryMirror(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "  ", want: ""},
		{in: "http://127.0.0.1:5000", want: "http://127.0.0.1:5000"},
		{in: "https://mirror.example.com/", want: "https://mirror.example.com"},
		{in: "127.0.0.1:5000", wantErr: true},
		{in: "ftp://mirror.example.com", wantErr: true},
		{in: "http://mirror.example.com/v2", wantErr: true},
		{in: "http://user:pass@mirror.example.com", wantErr: true},
		{in: "http://mirror.example.com?ns=x", wantErr: true},
	} {
		got, err := parseRegistryMirror(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRegistryMirror(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}

		if got != tt.want {
			t.Errorf("parseRegistryMirror(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
