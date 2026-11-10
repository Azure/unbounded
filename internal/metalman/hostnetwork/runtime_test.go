// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

const referenceRuntime = `ethernet enp49s0f0np0 02:00:00:00:00:01 -
ethernet enp49s0f1np1 02:00:00:00:00:02 -
bond bond0 802.3ad 100 layer3+4 - 1500 enp49s0f0np0,enp49s0f1np1
vlan bond0.64 bond0 64 -
address bond0 192.0.2.30/24
address bond0.64 198.51.100.30/24
route bond0 default 192.0.2.1
route bond0.64 203.0.113.0/24 198.51.100.1
route bond0.64 100.64.0.0/16 198.51.100.1
dns 192.0.2.53
dns 192.0.2.54
source bond0 192.0.2.30
`

func TestRenderRuntimeReference(t *testing.T) {
	n, l := reference()

	got, err := RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != referenceRuntime {
		t.Fatalf("runtime mismatch:\n%s\nwant:\n%s", got, referenceRuntime)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		for _, tok := range strings.Fields(line) {
			if !tokenPattern.MatchString(tok) {
				t.Errorf("unsafe token %q", tok)
			}
		}
	}
}

func TestRenderRuntimeDefaultsAndOptionals(t *testing.T) {
	n, l := reference()
	n.Bonds[0].Mode = ""
	n.Bonds[0].MIIMonitorInterval = nil
	n.Bonds[0].TransmitHashPolicy = ""
	n.Bonds[0].LACPRate = "fast"
	n.Bonds[0].MTU = nil
	n.VLANs[0].MTU = i32(1400)
	n.DNS = nil

	got, err := RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"ethernet enp49s0f0np0 02:00:00:00:00:01 -\n",
		"bond bond0 802.3ad 100 - fast - enp49s0f0np0,enp49s0f1np1\n",
		"vlan bond0.64 bond0 64 1400\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	if strings.Contains(string(got), "dns ") {
		t.Errorf("unexpected dns line:\n%s", got)
	}

	n.Bonds[0].MIIMonitorInterval = i32(0)
	n.Bonds[0].Mode = "active-backup"
	n.Bonds[0].LACPRate = ""

	got, err = RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(got), "bond bond0 active-backup 0 - - - enp49s0f0np0,enp49s0f1np1\n") {
		t.Errorf("unexpected bond line:\n%s", got)
	}

	// A member MTU renders when the bond sets the same MTU.
	n.Bonds[0].MTU = i32(9000)
	n.Ethernets[0].MTU = i32(9000)

	got, err = RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"ethernet enp49s0f0np0 02:00:00:00:00:01 9000\n",
		"bond bond0 active-backup 0 - - 9000 enp49s0f0np0,enp49s0f1np1\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderRuntimeSingle(t *testing.T) {
	n, l := single()

	got, err := RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	want := "ethernet eth0 02:00:00:00:00:0a -\naddress eth0 192.0.2.10/24\nroute eth0 default 192.0.2.1\nsource eth0 192.0.2.10\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderRuntimeValidates(t *testing.T) {
	n, l := reference()
	l.MAC = "02:00:00:00:00:09"

	if _, err := RenderRuntime(n, l); err == nil {
		t.Fatal("expected validation error")
	}

	if _, err := RenderRuntime(nil, l); err == nil {
		t.Fatal("expected error for nil network")
	}
}

func TestRenderRuntimeDistinctDottedNames(t *testing.T) {
	n, l := single()
	n.VLANs = []v1alpha3.VLANSpec{
		{Name: "eth0-64", ID: 64, Link: "eth0"},
		{Name: "eth0.64", ID: 65, Link: "eth0"},
	}

	got, err := RenderRuntime(n, l)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"vlan eth0-64 eth0 64 -\n", "vlan eth0.64 eth0 65 -\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
