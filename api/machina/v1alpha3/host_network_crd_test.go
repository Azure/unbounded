// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// hostNetworkNetbootYAML is the reference topology from
// designs/metalman-host-network.md: two ethernets in an 802.3ad bond with a
// VLAN on top, booted through VirtualMedia. Use netbootYAML to indent it.
const hostNetworkNetbootYAML = `
        image: ghcr.io/example/host@sha256:0000000000000000000000000000000000000000000000000000000000000000
        netbootImage: ghcr.io/example/netboot@sha256:0000000000000000000000000000000000000000000000000000000000000000
        bootProtocol: VirtualMedia
        redfish:
          url: https://192.0.2.1
          username: admin
          passwordRef:
            name: bmc
            namespace: default
        dhcpLeases:
        - ipv4: 203.0.113.10
          mac: 02:00:00:00:00:01
          subnetMask: 255.255.255.0
          gateway: 203.0.113.1
        network:
          ethernets:
          - name: enp1s0f0
            macAddress: 02:00:00:00:00:01
          - name: enp1s0f1
            macAddress: 02:00:00:00:00:02
          bonds:
          - name: bond0
            interfaces: [enp1s0f0, enp1s0f1]
            mode: 802.3ad
            transmitHashPolicy: layer3+4
            lacpRate: fast
            miiMonitorInterval: 100
            mtu: 1500
          vlans:
          - name: bond0.64
            id: 64
            link: bond0
            addresses: [203.0.113.10/24]
            gateway: 203.0.113.1
            routes:
            - to: 198.51.100.0/24
              via: 203.0.113.254
          dns: [192.0.2.53, 198.51.100.53]
`

// netbootYAML returns hostNetworkNetbootYAML indented by indent spaces.
func netbootYAML(indent int) string {
	return strings.ReplaceAll(hostNetworkNetbootYAML, "\n        ", "\n"+strings.Repeat(" ", indent))
}

func newHostNetworkMachineObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: host-1
spec:
  host:
    netboot:`+netbootYAML(6))
}

func networkPath(path ...string) []string {
	return append([]string{"spec", "host", "netboot", "network"}, path...)
}

// setListItemField sets a field of one element of a list of objects.
func setListItemField(value any, index int, field string, path ...string) func(*testing.T, map[string]any) {
	return func(t *testing.T, obj map[string]any) {
		t.Helper()

		items, found, err := unstructured.NestedSlice(obj, path...)
		if err != nil || !found || index >= len(items) {
			t.Fatalf("list %v not found: %v", path, err)
		}

		items[index].(map[string]any)[field] = value
		setField(t, obj, items, path...)
	}
}

func removeListItemField(index int, field string, path ...string) func(*testing.T, map[string]any) {
	return func(t *testing.T, obj map[string]any) {
		t.Helper()

		items, found, err := unstructured.NestedSlice(obj, path...)
		if err != nil || !found || index >= len(items) {
			t.Fatalf("list %v not found: %v", path, err)
		}

		delete(items[index].(map[string]any), field)
		setField(t, obj, items, path...)
	}
}

func TestHostNetworkMachineCreate(t *testing.T) {
	t.Parallel()

	ethernets := networkPath("ethernets")
	bonds := networkPath("bonds")
	vlans := networkPath("vlans")

	tests := []machineCreateCase{
		{name: "reference topology is accepted"},
		{
			name:   "network on legacy pxe is accepted",
			object: newHostNetworkLegacyPXEMachineObject,
		},
		{
			name:   "network requires VirtualMedia",
			mutate: setMachineField("HTTP", "spec", "host", "netboot", "bootProtocol"),
			want:   []string{"network requires bootProtocol VirtualMedia"},
		},
		{
			name:   "network requires VirtualMedia when bootProtocol defaults to PXE",
			mutate: removeMachineField("spec", "host", "netboot", "bootProtocol"),
			want:   []string{"network requires bootProtocol VirtualMedia"},
		},
		{
			name:   "network requires dhcpLeases",
			mutate: removeMachineField("spec", "host", "netboot", "dhcpLeases"),
			want:   []string{"network requires a dhcpLeases entry"},
		},
		{
			name:   "network requires a non-empty dhcpLeases",
			mutate: setMachineField([]any{}, "spec", "host", "netboot", "dhcpLeases"),
			want:   []string{"network requires a dhcpLeases entry"},
		},
		{
			name:   "ethernets are required",
			mutate: removeMachineField(ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets", "Required value"},
		},
		{
			name:   "bond member must be a declared ethernet",
			mutate: setListItemField([]any{"enp1s0f0", "enp9s0"}, 0, "interfaces", bonds...),
			want:   []string{"bond interfaces must reference declared ethernets"},
		},
		{
			name:   "legacy pxe bond member must be a declared ethernet",
			object: newHostNetworkLegacyPXEMachineObject,
			mutate: setListItemField([]any{"enp9s0"}, 0, "interfaces", "spec", "pxe", "network", "bonds"),
			want:   []string{"spec.pxe.network.bonds", "bond interfaces must reference declared ethernets"},
		},
		{
			name:   "legacy pxe vlan link must be declared",
			object: newHostNetworkLegacyPXEMachineObject,
			mutate: setListItemField("bond1", 0, "link", "spec", "pxe", "network", "vlans"),
			want:   []string{"spec.pxe.network.vlans", "vlan link must reference a declared ethernet or bond"},
		},
		{
			name:   "vlan link must be declared",
			mutate: setListItemField("bond1", 0, "link", vlans...),
			want:   []string{"vlan link must reference a declared ethernet or bond"},
		},
		{
			name: "vlan link may be an ethernet without bonds",
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, bonds...)
				setListItemField("enp1s0f0", 0, "link", vlans...)(t, obj)
			},
		},
		{
			name: "vlan link to a bond is rejected without bonds",
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, bonds...)
			},
			want: []string{"vlan link must reference a declared ethernet or bond"},
		},
		{
			name:   "lacpRate with active-backup is rejected",
			mutate: setListItemField("active-backup", 0, "mode", bonds...),
			want:   []string{"lacpRate requires mode 802.3ad"},
		},
		{
			name:   "lacpRate with the default mode is accepted",
			mutate: removeListItemField(0, "mode", bonds...),
		},
		{
			name: "active-backup without lacpRate is accepted",
			mutate: func(t *testing.T, obj map[string]any) {
				setListItemField("active-backup", 0, "mode", bonds...)(t, obj)
				removeListItemField(0, "lacpRate", bonds...)(t, obj)
			},
		},
		{
			name:   "unknown bond mode is rejected",
			mutate: setListItemField("balance-rr", 0, "mode", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].mode", "Unsupported value"},
		},
		{
			name:   "unknown transmit hash policy is rejected",
			mutate: setListItemField("layer4", 0, "transmitHashPolicy", bonds...),
			want:   []string{"transmitHashPolicy", "Unsupported value"},
		},
		{
			name:   "uppercase MAC is rejected",
			mutate: setListItemField("02:00:00:00:00:AB", 0, "macAddress", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].macAddress", "should match"},
		},
		{
			name:   "short MAC is rejected",
			mutate: setListItemField("02:00:00:00:01", 0, "macAddress", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].macAddress", "should match"},
		},
		{
			name:   "long MAC is rejected",
			mutate: setListItemField("02:00:00:00:00:01:02", 0, "macAddress", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].macAddress", "should match"},
		},
		{
			name:   "vlan id 0 is rejected",
			mutate: setListItemField(int64(0), 0, "id", vlans...),
			want:   []string{"spec.host.netboot.network.vlans[0].id", "greater than or equal to 1"},
		},
		{
			name:   "vlan id 4095 is rejected",
			mutate: setListItemField(int64(4095), 0, "id", vlans...),
			want:   []string{"spec.host.netboot.network.vlans[0].id", "less than or equal to 4094"},
		},
		{
			name:   "vlan id 4094 is accepted",
			mutate: setListItemField(int64(4094), 0, "id", vlans...),
		},
		{
			name:   "ethernet name with a leading digit is rejected",
			mutate: setListItemField("1eth", 0, "name", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].name", "should match"},
		},
		{
			name:   "ethernet name longer than 15 characters is rejected",
			mutate: setListItemField("enp1s0f0abcdefgh", 0, "name", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].name", "Too long"},
		},
		{
			name:   "ethernet name with a dot is rejected",
			mutate: setListItemField("enp1s0.f0", 0, "name", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[0].name", "should match"},
		},
		{
			name:   "bond name with a slash is rejected",
			mutate: setListItemField("bond/0", 0, "name", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].name", "should match"},
		},
		{
			name:   "duplicate ethernet names are rejected",
			mutate: setListItemField("enp1s0f0", 1, "name", ethernets...),
			want:   []string{"spec.host.netboot.network.ethernets[1]", "Duplicate value"},
		},
		{
			name:   "mtu below 576 is rejected",
			mutate: setListItemField(int64(575), 0, "mtu", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].mtu", "greater than or equal to 576"},
		},
		{
			name:   "mtu above 9216 is rejected",
			mutate: setListItemField(int64(9217), 0, "mtu", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].mtu", "less than or equal to 9216"},
		},
		{
			name:   "mtu bounds are inclusive",
			mutate: setListItemField(int64(9216), 0, "mtu", bonds...),
		},
		{
			name:   "miiMonitorInterval above 10000 is rejected",
			mutate: setListItemField(int64(10001), 0, "miiMonitorInterval", bonds...),
			want:   []string{"miiMonitorInterval", "less than or equal to 10000"},
		},
		{
			name:   "more than three dns entries are rejected",
			mutate: setMachineField([]any{"192.0.2.53", "192.0.2.54", "192.0.2.55", "192.0.2.56"}, networkPath("dns")...),
			want:   []string{"spec.host.netboot.network.dns", "Too many"},
		},
		{
			name:   "empty bond interfaces are rejected",
			mutate: setListItemField([]any{}, 0, "interfaces", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].interfaces", "at least 1"},
		},
		{
			name:   "duplicate bond interfaces are rejected",
			mutate: setListItemField([]any{"enp1s0f0", "enp1s0f0"}, 0, "interfaces", bonds...),
			want:   []string{"spec.host.netboot.network.bonds[0].interfaces[1]", "Duplicate value"},
		},
		{
			name:   "route requires via",
			mutate: setListItemField([]any{map[string]any{"to": "198.51.100.0/24"}}, 0, "routes", vlans...),
			want:   []string{"spec.host.netboot.network.vlans[0].routes[0].via", "Required value"},
		},
	}

	for i := range tests {
		if tests[i].object == nil {
			tests[i].object = newHostNetworkMachineObject
		}
	}

	runMachineCreateCases(t, tests)
}

func newHostNetworkLegacyPXEMachineObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: host-1
spec:
  pxe:`+netbootYAML(4))
}

func TestHostNetworkDefaults(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineCRD)

	obj := newHostNetworkMachineObject(t)
	removeListItemField(0, "mode", networkPath("bonds")...)(t, obj)
	removeListItemField(0, "miiMonitorInterval", networkPath("bonds")...)(t, obj)
	removeListItemField(0, "lacpRate", networkPath("bonds")...)(t, obj)

	assertErrors(t, v.validateCreate(obj), nil)

	bonds, _, err := unstructured.NestedSlice(obj, networkPath("bonds")...)
	if err != nil || len(bonds) != 1 {
		t.Fatalf("bonds = %v, %v", bonds, err)
	}

	bond := bonds[0].(map[string]any)
	if got := bond["mode"]; got != BondMode8023AD {
		t.Errorf("bond mode default = %v, want %q", got, BondMode8023AD)
	}

	if got := bond["miiMonitorInterval"]; got != int64(100) {
		t.Errorf("bond miiMonitorInterval default = %#v, want 100", got)
	}
}

func TestHostNetworkMachineOperationStatus(t *testing.T) {
	t.Parallel()

	// newCRDValidator fails the test if any CEL rule, including the host
	// network rules, exceeds the API server's estimated cost limit.
	v := newCRDValidator(t, machineOperationCRD)

	newObj := func(t *testing.T) map[string]any {
		t.Helper()

		return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: op-1
spec:
  machineRef: worker-01
  operationKind: HostReplace
status:
  targets:
  - machineRef: worker-01
    bootMedia:
      id: media-1
      phase: Staged
      machineUID: 6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10
      netboot:`+netbootYAML(8))
	}

	t.Run("reference topology is accepted", func(t *testing.T) {
		t.Parallel()

		assertErrors(t, v.validateCreate(newObj(t)), nil)
	})

	// The status snapshot copies a validated Machine, so it carries only the
	// per-field schema and the cheap PXESpec rules; the cross-reference rules
	// live on Machine only to fit the cost budget.
	t.Run("snapshot keeps per-field validation", func(t *testing.T) {
		t.Parallel()

		obj := newObj(t)
		targets, _, _ := unstructured.NestedSlice(obj, "status", "targets")
		target := targets[0].(map[string]any)
		ethernets, _, _ := unstructured.NestedSlice(target, "bootMedia", "netboot", "network", "ethernets")
		ethernets[0].(map[string]any)["macAddress"] = "02:00:00:00:00:AB"
		setField(t, target, ethernets, "bootMedia", "netboot", "network", "ethernets")
		setField(t, obj, targets, "status", "targets")

		assertErrors(t, v.validateCreate(obj), []string{"ethernets[0].macAddress", "should match"})
	})

	t.Run("snapshot keeps the PXESpec rules", func(t *testing.T) {
		t.Parallel()

		obj := newObj(t)
		targets, _, _ := unstructured.NestedSlice(obj, "status", "targets")
		unstructured.RemoveNestedField(targets[0].(map[string]any), "bootMedia", "netboot", "dhcpLeases")
		setField(t, obj, targets, "status", "targets")

		assertErrors(t, v.validateCreate(obj), []string{"network requires a dhcpLeases entry"})
	})
}
