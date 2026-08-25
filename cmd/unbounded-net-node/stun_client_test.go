// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func TestSTUNPublicIPDiscovererStopsOnCancellation(t *testing.T) {
	t.Parallel()

	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for STUN request: %v", err)
	}
	defer server.Close()

	requestReceived := make(chan struct{}, 1)

	go func() {
		request := make([]byte, 2048)
		if _, _, readErr := server.ReadFrom(request); readErr == nil {
			requestReceived <- struct{}{}
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)

	go func() {
		_, discoverErr := (stunPublicIPDiscoverer{}).DiscoverPublicIP(ctx, server.LocalAddr().String())
		result <- discoverErr
	}()

	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for STUN request")
	}

	cancel()

	select {
	case discoverErr := <-result:
		if !errors.Is(discoverErr, context.Canceled) {
			t.Fatalf("DiscoverPublicIP() error = %v, want context canceled", discoverErr)
		}
	case <-time.After(time.Second):
		t.Fatal("STUN discovery did not stop after cancellation")
	}
}

func TestPublicIPFromSTUNResponse(t *testing.T) {
	t.Parallel()

	transactionID := [stun.TransactionIDSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	otherTransactionID := transactionID
	otherTransactionID[0]++
	reservedTypeBits := buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("203.0.113.20"))
	reservedTypeBits[0] |= 0xc0
	undersizedAddress := buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("203.0.113.20"))
	undersizedAddress[23] = 7
	invalidAddressFamily := buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("203.0.113.20"))
	invalidAddressFamily[25] = 3
	mappedIPv4 := buildIPv6FamilySTUNResponse(
		t,
		stun.BindingSuccess,
		transactionID,
		netip.MustParseAddr("::ffff:203.0.113.20"),
	)

	tests := []struct {
		name          string
		raw           []byte
		transactionID [stun.TransactionIDSize]byte
		want          netip.Addr
		wantErr       string
	}{
		{name: "IPv4", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("203.0.113.20")), transactionID: transactionID, want: netip.MustParseAddr("203.0.113.20")},
		{name: "IPv6", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("2001:db8::20")), transactionID: transactionID, want: netip.MustParseAddr("2001:db8::20")},
		{name: "IPv4-mapped IPv6", raw: mappedIPv4, transactionID: transactionID, want: netip.MustParseAddr("203.0.113.20")},
		{name: "private IPv4", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("10.0.0.20")), transactionID: transactionID, want: netip.MustParseAddr("10.0.0.20")},
		{name: "carrier-grade NAT IPv4", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("100.64.0.20")), transactionID: transactionID, want: netip.MustParseAddr("100.64.0.20")},
		{name: "unique-local IPv6", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("fd00::20")), transactionID: transactionID, want: netip.MustParseAddr("fd00::20")},
		{name: "unspecified", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("0.0.0.0")), transactionID: transactionID, wantErr: "unusable mapped IP"},
		{name: "loopback", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("::1")), transactionID: transactionID, wantErr: "unusable mapped IP"},
		{name: "multicast", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("224.0.0.1")), transactionID: transactionID, wantErr: "unusable mapped IP"},
		{name: "link-local", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("fe80::1")), transactionID: transactionID, wantErr: "unusable mapped IP"},
		{name: "limited broadcast", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("255.255.255.255")), transactionID: transactionID, wantErr: "unusable mapped IP"},
		{name: "malformed", raw: []byte{1, 2, 3}, transactionID: transactionID, wantErr: "decode binding response"},
		{name: "wrong transaction", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, net.ParseIP("203.0.113.20")), transactionID: otherTransactionID, wantErr: "transaction ID"},
		{name: "error response", raw: buildSTUNResponse(t, stun.BindingError, transactionID, nil), transactionID: transactionID, wantErr: "unexpected binding response type"},
		{name: "missing address", raw: buildSTUNResponse(t, stun.BindingSuccess, transactionID, nil), transactionID: transactionID, wantErr: "XOR-MAPPED-ADDRESS"},
		{name: "reserved response type bits", raw: reservedTypeBits, transactionID: transactionID, wantErr: "reserved message type bits"},
		{name: "undersized address", raw: undersizedAddress, transactionID: transactionID, wantErr: "length is 7, want 8"},
		{name: "invalid address family", raw: invalidAddressFamily, transactionID: transactionID, wantErr: "XOR-MAPPED-ADDRESS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := publicIPFromSTUNResponse(tt.raw, tt.transactionID)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("publicIPFromSTUNResponse() error = %v", err)
			}

			if got != tt.want {
				t.Fatalf("publicIPFromSTUNResponse() = %s, want %s", got, tt.want)
			}
		})
	}
}

func buildSTUNResponse(t *testing.T, messageType stun.MessageType, transactionID [stun.TransactionIDSize]byte, ip net.IP) []byte {
	t.Helper()

	setters := []stun.Setter{messageType, stun.NewTransactionIDSetter(transactionID)}
	if ip != nil {
		setters = append(setters, stun.XORMappedAddress{IP: ip, Port: 51820})
	}

	message, err := stun.Build(setters...)
	if err != nil {
		t.Fatalf("build STUN response: %v", err)
	}

	return message.Raw
}

func buildIPv6FamilySTUNResponse(
	t *testing.T,
	messageType stun.MessageType,
	transactionID [stun.TransactionIDSize]byte,
	ip netip.Addr,
) []byte {
	t.Helper()

	if !ip.Is6() {
		t.Fatalf("mapped address %s is not IPv6", ip)
	}

	const magicCookie = 0x2112a442

	value := make([]byte, 20)
	binary.BigEndian.PutUint16(value[0:2], 2)
	binary.BigEndian.PutUint16(value[2:4], uint16(51820)^uint16(magicCookie>>16))

	xorMask := make([]byte, 16)
	binary.BigEndian.PutUint32(xorMask[0:4], magicCookie)
	copy(xorMask[4:], transactionID[:])

	address := ip.As16()
	for index, octet := range address {
		value[4+index] = octet ^ xorMask[index]
	}

	message, err := stun.Build(
		messageType,
		stun.NewTransactionIDSetter(transactionID),
		stun.RawAttribute{Type: stun.AttrXORMappedAddress, Value: value},
	)
	if err != nil {
		t.Fatalf("build IPv6-family STUN response: %v", err)
	}

	return message.Raw
}
