// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestInstallChairHolderReplacesStaleAddresses(t *testing.T) {
	t.Parallel()

	target, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create target host: %v", err)
	}

	defer func() { _ = target.Close() }() //nolint:errcheck // best-effort test cleanup

	caller, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create caller host: %v", err)
	}

	defer func() { _ = caller.Close() }() //nolint:errcheck // best-effort test cleanup

	stale := multiaddr.StringCast("/ip4/10.0.0.1/tcp/4001")
	caller.Peerstore().AddAddr(target.ID(), stale, peerstore.PermanentAddrTTL)
	fresh := "/ip4/10.0.0.2/tcp/4001/p2p/" + target.ID().String()

	if err := installChairHolder(caller.Peerstore(), chairs.Holder{
		PeerID:   ifaces.NodeID(target.ID().String()),
		P2PAddrs: []string{fresh},
	}); err != nil {
		t.Fatalf("installChairHolder: %v", err)
	}

	want := multiaddr.StringCast("/ip4/10.0.0.2/tcp/4001")
	if addrs := caller.Peerstore().Addrs(target.ID()); len(addrs) != 1 || !addrs[0].Equal(want) {
		t.Fatalf("peerstore addresses = %v, want [%s]", addrs, want)
	}
}
