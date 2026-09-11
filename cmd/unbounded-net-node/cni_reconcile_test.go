// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestCNIReconciliationDisablesAndRecoversWithoutMTUChange(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	cidrs := []string{"10.244.2.0/24"}

	health := &nodeHealthState{}
	if err := guardedWriteCNIConfig(context.Background(), cfg, cidrs, health); err != nil {
		t.Fatal(err)
	}

	state := &wireGuardState{
		clientset:        fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName}}),
		nodeName:         cfg.NodeName,
		nodePodCIDRs:     cidrs,
		cniConfigWritten: true,
		cniMTU:           cfg.MTU,
		manageCniPlugin:  true,
		healthState:      health,
	}
	cfg.MTU = 1200
	unsafe := true
	inspections := 0
	cfg.cniInspector = func(context.Context, string, []string) error {
		inspections++

		if unsafe {
			return errors.New("interface=eth0 address=10.244.1.8 outside assigned PodCIDRs")
		}

		return nil
	}

	originalMTU := ensureCNIBridgeMTUFunc
	originalConfigure := configureWireGuardFunc

	t.Cleanup(func() {
		ensureCNIBridgeMTUFunc = originalMTU
		configureWireGuardFunc = originalConfigure
	})

	ensureCNIBridgeMTUFunc = func(string, int, *unboundednetnetlink.NetlinkCache, bool) error { return nil }
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, _ []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		return nil
	}

	sites := newInformerWithObjects(toUnstructured(t, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
	}))
	empty := newInformerWithObjects()
	reconcile := func() error {
		return updateWireGuardFromSlices(context.Background(), nil, sites, empty, empty, empty, empty, empty, empty, cfg, "site-a", "private", "public", true, state)
	}

	err := reconcile()

	var guardErr *cniGuardError
	if !errors.As(err, &guardErr) || inspections != 1 {
		t.Fatalf("MTU update bypassed CNI guard: inspections=%d err=%v", inspections, err)
	}

	if _, err := os.Stat(cniConfigPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe MTU rewrite left active config: %v", err)
	}

	unsafe = false
	state.cniMTU = cfg.MTU

	if err := reconcile(); err != nil {
		t.Fatalf("unchanged MTU did not recover blocked CNI: %v", err)
	}

	if inspections != 2 {
		t.Fatalf("recovery skipped inspection: %d", inspections)
	}

	if ready, reason := health.cniReadiness(); !ready {
		t.Fatalf("recovery left CNI unready: %s", reason)
	}

	if _, err := os.Stat(cniConfigPath(cfg)); err != nil {
		t.Fatalf("recovery failed to publish config: %v", err)
	}

	if _, err := os.Stat(cniDisabledPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left disabled backup: %v", err)
	}
}
