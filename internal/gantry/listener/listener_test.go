// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package listener

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		wantNetwork string
		wantAddress string
		wantErr     bool
	}{
		{name: "TCP", endpoint: "127.0.0.1:5000", wantNetwork: "tcp", wantAddress: "127.0.0.1:5000"},
		{name: "Unix", endpoint: "unix:///run/gantry/mirror.sock", wantNetwork: "unix", wantAddress: "/run/gantry/mirror.sock"},
		{name: "Relative Unix", endpoint: "unix://mirror.sock", wantErr: true},
		{name: "Missing TCP port", endpoint: "127.0.0.1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network, address, err := Parse(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
			}

			if network != tt.wantNetwork || address != tt.wantAddress {
				t.Fatalf("Parse(%q) = (%q, %q), want (%q, %q)", tt.endpoint, network, address, tt.wantNetwork, tt.wantAddress)
			}
		})
	}
}

func TestListenUnixRefusesToReplaceFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gantry.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := Listen("unix://" + path); err == nil {
		t.Fatal("Listen replaced a non-socket file")
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "gantry.sock")

	first, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first listener: %v", err)
	}

	second, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer second.Close() //nolint:errcheck // test cleanup

	if _, ok := second.(*net.UnixListener); !ok {
		t.Fatalf("Listen(%q) returned %T, want *net.UnixListener", endpoint, second)
	}
}
