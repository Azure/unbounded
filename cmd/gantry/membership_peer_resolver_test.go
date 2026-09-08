// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"testing"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

func TestMembershipPeerIDResolverInstallsPodAddresses(t *testing.T) {
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

	announced := "/ip4/10.64.1.23/tcp/4001/p2p/" + target.ID().String()
	members := fakes.NewMembers("self", ifaces.Node{
		ID:       "target-node",
		PeerID:   target.ID().String(),
		P2PAddrs: []string{announced},
	})
	caller.Peerstore().AddAddr(
		target.ID(),
		multiaddr.StringCast("/ip4/127.0.0.1/tcp/4001"),
		peerstore.PermanentAddrTTL,
	)

	resolve := membershipPeerIDResolver(members, caller.Peerstore(), slog.Default())

	got, ok := resolve("target-node")
	if !ok || got != target.ID() {
		t.Fatalf("resolved peer = %q, %v; want %q, true", got, ok, target.ID())
	}

	want := multiaddr.StringCast("/ip4/10.64.1.23/tcp/4001")
	if addrs := caller.Peerstore().Addrs(target.ID()); len(addrs) != 1 || !addrs[0].Equal(want) {
		t.Fatalf("peerstore addresses = %v, want [%s]", addrs, want)
	}
}

func TestMembershipPeerIDResolverRejectsMismatchedAddressIdentity(t *testing.T) {
	t.Parallel()

	target, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create target host: %v", err)
	}

	defer func() { _ = target.Close() }() //nolint:errcheck // best-effort test cleanup

	other, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create other host: %v", err)
	}

	defer func() { _ = other.Close() }() //nolint:errcheck // best-effort test cleanup

	caller, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create caller host: %v", err)
	}

	defer func() { _ = caller.Close() }() //nolint:errcheck // best-effort test cleanup

	members := fakes.NewMembers("self", ifaces.Node{
		ID:       "target-node",
		PeerID:   target.ID().String(),
		P2PAddrs: []string{"/ip4/10.64.1.23/tcp/4001/p2p/" + other.ID().String()},
	})
	known := multiaddr.StringCast("/ip4/127.0.0.1/tcp/4001")
	caller.Peerstore().AddAddr(target.ID(), known, peerstore.PermanentAddrTTL)

	resolve := membershipPeerIDResolver(members, caller.Peerstore(), slog.Default())

	got, ok := resolve("target-node")
	if !ok || got != target.ID() {
		t.Fatalf("resolved peer = %q, %v; want %q, true", got, ok, target.ID())
	}

	if addrs := caller.Peerstore().Addrs(target.ID()); len(addrs) != 1 || !addrs[0].Equal(known) {
		t.Fatalf("peerstore addresses = %v, want existing address [%s]", addrs, known)
	}
}

func TestMembershipPeerIDResolverMiss(t *testing.T) {
	t.Parallel()

	members := fakes.NewMembers("self")
	resolve := membershipPeerIDResolver(members, nil, slog.Default())

	if got, ok := resolve(ifaces.NodeID(peer.ID("missing"))); ok || got != "" {
		t.Fatalf("resolved missing peer = %q, %v; want empty, false", got, ok)
	}
}

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
