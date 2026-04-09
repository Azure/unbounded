// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestParseLLDPNeighbors(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantCount  int
		wantChecks []struct {
			iface   string
			chassis string
			port    string
			portD   string
			sysName string
			sysDesc string
			mgmt    string
		}
	}{
		{
			name:      "lldpctl-1 (2 interfaces)",
			file:      "testdata/lldpctl-1.txt",
			wantCount: 2,
			wantChecks: []struct {
				iface   string
				chassis string
				port    string
				portD   string
				sysName string
				sysDesc string
				mgmt    string
			}{
				{
					iface:   "eno1",
					chassis: "e7:db:10:b3:ca:34",
					port:    "27",
					portD:   "sample-port-desc-1",
					sysName: "sample-upstream-switch",
					sysDesc: "sample switch OS description",
				},
				{
					iface:   "eno2",
					chassis: "e7:db:10:b3:ca:34",
					port:    "28",
					portD:   "sample-port-desc-2",
					sysName: "sample-upstream-switch",
					sysDesc: "sample switch OS description",
				},
			},
		},
		{
			name:      "lldpctl-2 (32 interfaces)",
			file:      "testdata/lldpctl-2.txt",
			wantCount: 32,
			wantChecks: []struct {
				iface   string
				chassis string
				port    string
				portD   string
				sysName string
				sysDesc string
				mgmt    string
			}{
				{
					iface:   "be0p0",
					chassis: "e7:db:10:b3:ca:35",
					port:    "etp7a",
					portD:   "sample-port-desc-1",
					sysName: "sample-upstream-switch-1",
					mgmt:    "d38:4c15:01e7:8568:a77b:8c34:53eb:568f",
				},
				{
					iface:   "be0p1",
					chassis: "e7:db:10:b3:ca:36",
					port:    "etp7a",
					portD:   "sample-port-desc-2",
					sysName: "sample-upstream-switch-2",
					mgmt:    "5d21:e784:26e0:450a:a917:702d:bd09:65f1",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("failed to read testdata: %v", err)
			}

			records, err := parseLLDPNeighbors("test-host", data)
			if err != nil {
				t.Fatalf("parseLLDPNeighbors: %v", err)
			}

			if len(records) != tc.wantCount {
				t.Fatalf("expected %d records, got %d", tc.wantCount, len(records))
			}

			byIface := make(map[string]LLDPNeighborAttributes)

			for _, r := range records {
				if r.HostIdentifier != "test-host" {
					t.Errorf("unexpected host identifier %q", r.HostIdentifier)
				}

				var attrs LLDPNeighborAttributes
				if err := json.Unmarshal(r.Attributes, &attrs); err != nil {
					t.Fatalf("unmarshal attributes for %s: %v", r.LocalInterface, err)
				}

				byIface[r.LocalInterface] = attrs
			}

			for _, check := range tc.wantChecks {
				attrs, ok := byIface[check.iface]
				if !ok {
					t.Errorf("missing record for interface %s", check.iface)

					continue
				}

				if attrs.ChassisID != check.chassis {
					t.Errorf("%s: ChassisID = %q, want %q", check.iface, attrs.ChassisID, check.chassis)
				}

				if attrs.PortID != check.port {
					t.Errorf("%s: PortID = %q, want %q", check.iface, attrs.PortID, check.port)
				}

				if attrs.PortDescription != check.portD {
					t.Errorf("%s: PortDescription = %q, want %q", check.iface, attrs.PortDescription, check.portD)
				}

				if attrs.SystemName != check.sysName {
					t.Errorf("%s: SystemName = %q, want %q", check.iface, attrs.SystemName, check.sysName)
				}

				if check.sysDesc != "" && attrs.SystemDescription != check.sysDesc {
					t.Errorf("%s: SystemDescription = %q, want %q", check.iface, attrs.SystemDescription, check.sysDesc)
				}

				if check.mgmt != "" && attrs.MgmtAddresses != check.mgmt {
					t.Errorf("%s: MgmtAddresses = %q, want %q", check.iface, attrs.MgmtAddresses, check.mgmt)
				}
			}
		})
	}
}
