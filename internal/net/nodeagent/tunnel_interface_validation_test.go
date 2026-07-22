// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeagent

import (
	"strings"
	"testing"
)

func TestValidateTunnelInterfaceNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		geneve     string
		vxlan      string
		ipip       string
		wantErrSub string
	}{
		{
			name:   "defaults",
			geneve: "geneve0", vxlan: "vxlan0", ipip: "ipip0",
		},
		{
			name:   "custom-valid",
			geneve: "gnxa", vxlan: "vxa", ipip: "ipa",
		},
		{
			name:   "max-length-15",
			geneve: "g123456789abcde", vxlan: "v123456789abcde", ipip: "i123456789abcde",
		},
		{
			name:   "empty-geneve",
			geneve: "", vxlan: "vxlan0", ipip: "ipip0",
			wantErrSub: "--geneve-interface must not be empty",
		},
		{
			name:   "empty-vxlan",
			geneve: "geneve0", vxlan: "", ipip: "ipip0",
			wantErrSub: "--vxlan-interface must not be empty",
		},
		{
			name:   "empty-ipip",
			geneve: "geneve0", vxlan: "vxlan0", ipip: "",
			wantErrSub: "--ipip-interface must not be empty",
		},
		{
			name:   "too-long",
			geneve: "g123456789abcdef", vxlan: "vxlan0", ipip: "ipip0",
			wantErrSub: "too long",
		},
		{
			name:   "collide-unbounded0-geneve",
			geneve: "unbounded0", vxlan: "vxlan0", ipip: "ipip0",
			wantErrSub: "reserved device name",
		},
		{
			name:   "collide-unbounded0-vxlan",
			geneve: "geneve0", vxlan: "unbounded0", ipip: "ipip0",
			wantErrSub: "reserved device name",
		},
		{
			name:   "collide-unbounded0-ipip",
			geneve: "geneve0", vxlan: "vxlan0", ipip: "unbounded0",
			wantErrSub: "reserved device name",
		},
		{
			name:   "duplicate-geneve-vxlan",
			geneve: "shared", vxlan: "shared", ipip: "ipip0",
			wantErrSub: "--geneve-interface and --vxlan-interface cannot both be",
		},
		{
			name:   "duplicate-geneve-ipip",
			geneve: "shared", vxlan: "vxlan0", ipip: "shared",
			wantErrSub: "--geneve-interface and --ipip-interface cannot both be",
		},
		{
			name:   "duplicate-vxlan-ipip",
			geneve: "geneve0", vxlan: "shared", ipip: "shared",
			wantErrSub: "--vxlan-interface and --ipip-interface cannot both be",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateTunnelInterfaceNames(tc.geneve, tc.vxlan, tc.ipip)

			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}

			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErrSub, err.Error())
			}
		})
	}
}

func TestValidateWireGuardInterfacePrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		prefix     string
		wantErrSub string
	}{
		{name: "default", prefix: "wg"},
		{name: "single-char", prefix: "w"},
		{name: "ten-bytes-max", prefix: "abcdefghij"},
		{
			name: "empty", prefix: "",
			wantErrSub: "--wireguard-interface-prefix must not be empty",
		},
		{
			name: "too-long-11", prefix: "abcdefghijk",
			wantErrSub: "too long",
		},
		{
			name: "contains-slash", prefix: "w/g",
			wantErrSub: "must not contain '/'",
		},
		{
			name: "collide-unbounded0", prefix: "unbounded0",
			wantErrSub: "reserved device name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateWireGuardInterfacePrefix(tc.prefix)

			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}

			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErrSub, err.Error())
			}
		})
	}
}
