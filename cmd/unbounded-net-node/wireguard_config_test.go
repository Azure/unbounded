// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"github.com/vishvananda/netlink"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestWireGuardTeardownFailureRetainsGatewayGeneration(t *testing.T) {
	originalList, originalDelete := listWireGuardLinks, deleteWireGuardLink

	t.Cleanup(func() {
		listWireGuardLinks, deleteWireGuardLink = originalList, originalDelete
	})

	cfg := &config{WireGuardInterfacePrefix: "retiretestwg", WireGuardPort: 51820}
	state := &wireGuardState{
		linkManager:  unboundednetnetlink.NewLinkManager("no-such-test-wg"),
		gatewayNames: map[string]string{"retiretestwg51821": "old-gateway"},
	}
	injected := errors.New("injected teardown failure")

	for _, mode := range []string{"list", "delete"} {
		t.Run(mode, func(t *testing.T) {
			lifecycle := newGatewayInformerLifecycle(newGatewayLifecycleClient(), 0)
			t.Cleanup(lifecycle.close)

			snapshot, _, err := lifecycle.prepare(t.Context(), true)
			if err != nil {
				t.Fatal(err)
			}

			lifecycle.setRequired(false)

			cleanupFails := true
			listWireGuardLinks = func() ([]netlink.Link, error) {
				if cleanupFails && mode == "list" {
					return nil, injected
				}

				return []netlink.Link{&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "retiretestwg51821"}, LinkType: "wireguard"}}, nil
			}
			deleteWireGuardLink = func(netlink.Link) error {
				if cleanupFails {
					return injected
				}

				return nil
			}

			reconcile := func() error {
				err := configureWireGuard(t.Context(), cfg, "", nil, nil, "local", nil, nil, nil,
					nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, state, nil)
				if err == nil {
					lifecycle.retire(snapshot.generation)
				}

				return err
			}
			if err := reconcile(); !errors.Is(err, injected) {
				t.Fatalf("cleanup swallowed failure: %v", err)
			}

			if got := lifecycle.snapshot(); !got.available || got.generation != snapshot.generation {
				t.Fatal("failed cleanup retired synchronized caches")
			}

			if ready, _ := lifecycle.readiness(); ready {
				t.Fatal("failed cleanup marked retirement ready")
			}

			cleanupFails = false

			if err := reconcile(); err != nil {
				t.Fatal(err)
			}

			if lifecycle.snapshot().available {
				t.Fatal("successful cleanup retained caches")
			}
		})
	}
}
