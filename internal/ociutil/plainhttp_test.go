// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociutil

import (
	"testing"

	"oras.land/oras-go/v2/registry/remote"
)

func TestConfigurePlainHTTP(t *testing.T) {
	tests := []struct {
		name      string
		imageRef  string
		wantPlain bool
	}{
		// Loopback addresses.
		{name: "localhost", imageRef: "localhost/repo", wantPlain: true},
		{name: "localhost with port", imageRef: "localhost:5000/repo", wantPlain: true},
		{name: "127.0.0.1", imageRef: "127.0.0.1/repo", wantPlain: true},
		{name: "127.0.0.1 with port", imageRef: "127.0.0.1:5000/repo", wantPlain: true},
		{name: "IPv6 loopback", imageRef: "[::1]:5000/repo", wantPlain: true},

		// Private IPs (RFC 1918).
		{name: "10.x.x.x", imageRef: "10.0.0.1/repo", wantPlain: true},
		{name: "10.x.x.x with port", imageRef: "10.0.0.1:5555/repo", wantPlain: true},
		{name: "172.16.x.x", imageRef: "172.16.0.1/repo", wantPlain: true},
		{name: "172.16.x.x with port", imageRef: "172.16.0.1:5555/repo", wantPlain: true},
		{name: "172.31.x.x", imageRef: "172.31.255.255:5555/repo", wantPlain: true},
		{name: "192.168.x.x", imageRef: "192.168.0.1/repo", wantPlain: true},
		{name: "192.168.x.x with port", imageRef: "192.168.200.1:5555/repo", wantPlain: true},

		// Public addresses — should NOT be plain HTTP.
		{name: "public registry", imageRef: "registry.example.com/repo", wantPlain: false},
		{name: "docker.io", imageRef: "docker.io/library/ubuntu", wantPlain: false},
		{name: "public IP", imageRef: "8.8.8.8:5000/repo", wantPlain: false},
		{name: "172.32.x.x (not private)", imageRef: "172.32.0.1:5555/repo", wantPlain: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := remote.NewRepository(tt.imageRef)
			if err != nil {
				t.Fatalf("NewRepository(%q): %v", tt.imageRef, err)
			}

			ConfigurePlainHTTP(repo)

			if repo.PlainHTTP != tt.wantPlain {
				t.Errorf("ConfigurePlainHTTP(%q): PlainHTTP = %v, want %v",
					tt.imageRef, repo.PlainHTTP, tt.wantPlain)
			}
		})
	}
}
