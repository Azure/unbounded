// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"slices"
	"testing"
)

func TestMSSClampRule(t *testing.T) {
	tests := []struct {
		name      string
		fabricMTU int
		ipv6      bool
		wantMSS   string
		wantRange string
	}{
		{name: "IPv4", fabricMTU: 1280, wantMSS: "1240", wantRange: "1241:65535"},
		{name: "IPv6", fabricMTU: 1280, ipv6: true, wantMSS: "1220", wantRange: "1221:65535"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := mssClampRule(tt.fabricMTU, tt.ipv6)

			if !slices.Contains(rule, tt.wantMSS) {
				t.Fatalf("rule %v does not set MSS %s", rule, tt.wantMSS)
			}

			if !slices.Contains(rule, tt.wantRange) {
				t.Fatalf("rule %v does not match MSS range %s", rule, tt.wantRange)
			}

			if slices.Contains(rule, "-i") || slices.Contains(rule, "-o") {
				t.Fatalf("rule %v should apply across all forwarded interfaces", rule)
			}
		})
	}
}
