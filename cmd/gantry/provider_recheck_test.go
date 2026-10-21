// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

type recheckMetadataDialer struct {
	errors map[string]error
}

func (d *recheckMetadataDialer) HeadFromPeer(_ context.Context, addr string, _ ifaces.OriginRef) (int64, string, error) {
	return 1, "application/octet-stream", d.errors[addr]
}

func TestNF5UsabilityRecheckAllowsFallbackForStaleProvider(t *testing.T) {
	ref := providerRecheckRef()
	dht := fakes.NewDHT()

	dht.Inject(ref.Digest, ifaces.Provider{NodeID: "stale", Addr: "stale:5001"})

	dialer := &recheckMetadataDialer{errors: map[string]error{"stale:5001": errors.New("unreachable")}}
	controller := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 },
		Recheck:       newUsableProviderRecheck(dht, dialer),
	})

	proceed, release, err := controller.Allow(context.Background(), ref, 0)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if !proceed {
		t.Fatal("Allow declined fallback for stale provider")
	}

	if release == nil {
		t.Fatal("Allow release is nil")
	}

	release()
}

func TestNF5UsabilityRecheckDeclinesFallbackForUsableProvider(t *testing.T) {
	ref := providerRecheckRef()
	dht := fakes.NewDHT()

	dht.Inject(ref.Digest, ifaces.Provider{NodeID: "fresh", Addr: "fresh:5001"})
	controller := mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
		Inflight:      inflight.New(inflight.DefaultStalls(), nil),
		InBootstrap:   func() bool { return false },
		HealthyEnough: func() bool { return true },
		ClusterSize:   func() int { return 1 },
		Recheck:       newUsableProviderRecheck(dht, &recheckMetadataDialer{}),
	})

	proceed, release, err := controller.Allow(context.Background(), ref, 0)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if proceed {
		t.Fatal("Allow proceeded despite usable provider")
	}

	if release != nil {
		t.Fatal("Allow returned release after declining fallback")
	}
}

func providerRecheckRef() ifaces.OriginRef {
	return ifaces.OriginRef{
		Registry:   "registry.example.com",
		Repository: "repo/image",
		Digest:     digest.MustParse("sha256:3434343434343434343434343434343434343434343434343434343434343434"),
		Kind:       ifaces.KindBlob,
	}
}
