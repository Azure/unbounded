// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsWireGuardInterface(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		iface  string
		expect bool
	}{
		{name: "wg51820", iface: "wg51820", expect: true},
		{name: "wg51821", iface: "wg51821", expect: true},
		{name: "wg0", iface: "wg0", expect: true},
		{name: "wg99999", iface: "wg99999", expect: true},
		{name: "not wg: eth0", iface: "eth0", expect: false},
		{name: "not wg: lo", iface: "lo", expect: false},
		{name: "not wg: geneve0", iface: "geneve0", expect: false},
		{name: "bare wg", iface: "wg", expect: false},
		{name: "wg with letters", iface: "wgfoo", expect: false},
		{name: "wg with mixed", iface: "wg123abc", expect: false},
		{name: "empty", iface: "", expect: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expect, isWireGuardInterface(tt.iface))
		})
	}
}

func TestKnownOverlayInterfaces(t *testing.T) {
	t.Parallel()

	// Verify the known interfaces list contains the expected entries.
	expected := []string{"geneve0", "vxlan0", "ipip0", "unbounded0", "cbr0"}
	assert.Equal(t, expected, knownOverlayInterfaces)
}
