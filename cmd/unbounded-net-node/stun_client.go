// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/pion/stun/v3"
	"k8s.io/klog/v2"
)

type stunPublicIPDiscoverer struct{}

func (stunPublicIPDiscoverer) DiscoverPublicIP(ctx context.Context, server string) (netip.Addr, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", server)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("dial: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			klog.V(4).Infof("Failed to close STUN connection: %v", closeErr)
		}
	}()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return netip.Addr{}, fmt.Errorf("set connection deadline: %w", err)
		}
	}

	stopCancellation := context.AfterFunc(ctx, func() {
		if err := conn.SetDeadline(time.Now()); err != nil {
			klog.V(4).Infof("Failed to interrupt STUN connection: %v", err)
		}
	})
	defer stopCancellation()

	request, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("build binding request: %w", err)
	}

	if _, err := conn.Write(request.Raw); err != nil {
		return netip.Addr{}, fmt.Errorf("send binding request: %w", err)
	}

	response := make([]byte, 2048)

	read, err := conn.Read(response)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return netip.Addr{}, ctxErr
		}

		return netip.Addr{}, fmt.Errorf("read binding response: %w", err)
	}

	return publicIPFromSTUNResponse(response[:read], request.TransactionID)
}

func publicIPFromSTUNResponse(raw []byte, transactionID [stun.TransactionIDSize]byte) (netip.Addr, error) {
	response := &stun.Message{Raw: raw}
	if err := response.Decode(); err != nil {
		return netip.Addr{}, fmt.Errorf("decode binding response: %w", err)
	}

	if response.TransactionID != transactionID {
		return netip.Addr{}, fmt.Errorf("binding response transaction ID does not match request")
	}

	if raw[0]&0xc0 != 0 {
		return netip.Addr{}, fmt.Errorf("binding response has non-zero reserved message type bits")
	}

	if response.Type != stun.BindingSuccess {
		return netip.Addr{}, fmt.Errorf("unexpected binding response type %s", response.Type)
	}

	encodedAddress, err := response.Get(stun.AttrXORMappedAddress)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read XOR-MAPPED-ADDRESS: %w", err)
	}

	var mappedAddress stun.XORMappedAddress
	if err := mappedAddress.GetFrom(response); err != nil {
		return netip.Addr{}, fmt.Errorf("read XOR-MAPPED-ADDRESS: %w", err)
	}

	expectedLength := 4 + len(mappedAddress.IP)
	if len(encodedAddress) != expectedLength {
		return netip.Addr{}, fmt.Errorf("XOR-MAPPED-ADDRESS length is %d, want %d", len(encodedAddress), expectedLength)
	}

	publicIP, ok := netip.AddrFromSlice(mappedAddress.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("binding response contains invalid mapped IP %q", mappedAddress.IP)
	}

	publicIP = publicIP.Unmap()
	if !publicIP.IsGlobalUnicast() {
		return netip.Addr{}, fmt.Errorf("binding response contains an unusable mapped IP")
	}

	return publicIP, nil
}
