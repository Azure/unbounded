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
		{name: "zoned bracketed IPv6", host: "[fe80::1%eth0]", want: "[fe80::1%eth0]:22"},
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
