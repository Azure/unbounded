// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import "testing"

func TestHostPortZonedIPv6AndEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "zoned bare IPv6", host: "fe80::1%eth0", want: "[fe80::1%eth0]:22"},
		{name: "zoned bracketed IPv6", host: "[fe80::1%eth0]", want: "[fe80::1%eth0]:22"},
		{name: "zoned bracketed IPv6 with port", host: "[fe80::1%eth0]:22", want: "[fe80::1%eth0]:22"},
		{name: "IPv4 literal", host: "192.0.2.10", want: "192.0.2.10:22"},
		{name: "hostname with port", host: "example.test:2222", want: "example.test:2222"},
		{name: "empty host", host: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hostPort(tt.host); got != tt.want {
				t.Fatalf("hostPort(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}
