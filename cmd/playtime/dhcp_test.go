// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestSteerHTTPBootInjectsOptions(t *testing.T) {
	t.Parallel()

	state := &bootState{}
	state.set(true, "http://192.0.2.10:8000/images/boot.efi")

	r := &dhcpRelay{
		overlayIP: net.IPv4(172, 31, 99, 2).To4(),
		boot:      state,
	}

	msg, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("new dhcp msg: %v", err)
	}

	r.steerHTTPBoot(msg)

	if got := msg.ClassIdentifier(); got != "HTTPClient" {
		t.Fatalf("option 60 = %q, want HTTPClient", got)
	}

	want := "http://172.31.99.2:8090/images/boot.efi"
	if got := msg.BootFileNameOption(); got != want {
		t.Fatalf("option 67 = %q, want %q", got, want)
	}
}

func TestSteerHTTPBootNoOpWhenDisabled(t *testing.T) {
	t.Parallel()

	state := &bootState{} // httpBoot false.

	r := &dhcpRelay{
		overlayIP: net.IPv4(172, 31, 99, 2).To4(),
		boot:      state,
	}

	msg, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("new dhcp msg: %v", err)
	}

	r.steerHTTPBoot(msg)

	if got := msg.ClassIdentifier(); got != "" {
		t.Fatalf("option 60 = %q, want empty (no steering)", got)
	}

	if got := msg.BootFileNameOption(); got != "" {
		t.Fatalf("option 67 = %q, want empty (no steering)", got)
	}
}
