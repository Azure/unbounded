// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package peeraddr

import (
	"net/http/httptest"
	"testing"
)

func TestHost(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"ipv4", "10.0.1.10:1234", "10.0.1.10"},
		{"ipv6", "[2001:db8::1]:1234", "2001:db8::1"},
		{"missing port", "10.0.1.10", "10.0.1.10"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote

			if got := Host(r); got != tt.want {
				t.Fatalf("Host() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHostIgnoresForwardingHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.30:5555"
	r.Header.Set("X-Forwarded-For", "192.0.2.99, 192.0.2.31")
	r.Header.Set("X-Real-IP", "192.0.2.98")
	r.Header.Set("Forwarded", "for=192.0.2.97")

	if got := Host(r); got != "192.0.2.30" {
		t.Fatalf("Host() = %q, want direct peer 192.0.2.30", got)
	}
}
