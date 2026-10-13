// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestWaitForAdoptedNodePodCIDRsAndConfigureRejectsChangedAssignment(t *testing.T) {
	const nodeName = "node-a"

	tests := []struct {
		name    string
		adopted nodeAssignment
		current *corev1.Node
	}{
		{
			name:    "gateway pool assigned before GET",
			adopted: nodeAssignment{site: "site-a", nodeUID: "uid-a"},
			current: assignedBootstrapNode(nodeName, "uid-a", "site-a", "pool-a", "10.244.1.0/24"),
		},
		{
			name:    "site changed before GET",
			adopted: nodeAssignment{site: "site-a", nodeUID: "uid-a"},
			current: assignedBootstrapNode(nodeName, "uid-a", "site-b", "", "10.244.1.0/24"),
		},
		{
			name:    "node replaced before GET",
			adopted: nodeAssignment{site: "site-a", nodeUID: "uid-a"},
			current: assignedBootstrapNode(nodeName, "uid-b", "site-a", "", "10.244.1.0/24"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := adoptedBootstrapTestConfig(t, nodeName)
			healthState := &nodeHealthState{}
			healthState.beginManagedCNI(cfg.BridgeName)

			_, err := waitForAdoptedNodePodCIDRsAndConfigure(t.Context(), fake.NewClientset(tt.current), cfg, healthState, tt.adopted)
			if !errors.Is(err, errAssignmentChanged) {
				t.Fatalf("error = %v, want assignment changed", err)
			}

			if _, statErr := os.Stat(cniConfigPath(cfg)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("CNI config was written for changed assignment: %v", statErr)
			}
		})
	}
}

func TestWaitForAdoptedNodePodCIDRsAndConfigureUnchangedAssignment(t *testing.T) {
	const nodeName = "node-a"

	node := assignedBootstrapNode(nodeName, "uid-a", "site-a", "", "10.244.1.0/24")
	cfg := adoptedBootstrapTestConfig(t, nodeName)
	healthState := &nodeHealthState{}
	healthState.beginManagedCNI(cfg.BridgeName)

	adopted := nodeAssignment{site: "site-a", nodeUID: node.UID}

	podCIDRs, err := waitForAdoptedNodePodCIDRsAndConfigure(t.Context(), fake.NewClientset(node), cfg, healthState, adopted)
	if err != nil {
		t.Fatal(err)
	}

	if len(podCIDRs) != 1 || podCIDRs[0] != "10.244.1.0/24" {
		t.Fatalf("podCIDRs = %v", podCIDRs)
	}

	if _, err := os.Stat(cniConfigPath(cfg)); err != nil {
		t.Fatalf("CNI config was not written: %v", err)
	}
}

func TestWaitForAdoptedNodePodCIDRsAndConfigureRejectsWatchChangeWithoutPodCIDRs(t *testing.T) {
	const nodeName = "node-a"

	node := assignedBootstrapNode(nodeName, "uid-a", "site-a", "", "")
	client := fake.NewClientset(node)
	watcher := watch.NewFake()
	watchStarted := make(chan struct{})

	client.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
		close(watchStarted)

		return true, watcher, nil
	})

	go func() {
		<-watchStarted

		changed := node.DeepCopy()
		changed.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = "pool-a"
		changed.ResourceVersion = "2"
		watcher.Modify(changed)
	}()

	cfg := adoptedBootstrapTestConfig(t, nodeName)
	adopted := nodeAssignment{site: "site-a", nodeUID: node.UID}

	_, err := waitForAdoptedNodePodCIDRsAndConfigure(t.Context(), client, cfg, &nodeHealthState{}, adopted)
	if !errors.Is(err, errAssignmentChanged) {
		t.Fatalf("error = %v, want assignment changed", err)
	}
}

func TestWaitForAdoptedNodePodCIDRsAndConfigureRechecksAfterCNIRetry(t *testing.T) {
	const nodeName = "node-a"

	node := assignedBootstrapNode(nodeName, "uid-a", "site-a", "", "10.244.1.0/24")
	client := fake.NewClientset(node)
	cfg := adoptedBootstrapTestConfig(t, nodeName)
	cfg.cniRetryInterval = time.Millisecond

	var inspections atomic.Int32

	cfg.cniInspector = func(ctx context.Context, _ string, _ []string) error {
		inspections.Add(1)

		current, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		current.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = "pool-a"
		if _, err := client.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return err
		}

		return errors.New("retry CNI inspection")
	}

	existing := []byte(`{"cniVersion":"0.4.0","name":"existing","plugins":[]}`)
	if err := os.WriteFile(cniConfigPath(cfg), existing, 0o644); err != nil {
		t.Fatal(err)
	}

	adopted := nodeAssignment{site: "site-a", nodeUID: node.UID}

	_, err := waitForAdoptedNodePodCIDRsAndConfigure(t.Context(), client, cfg, &nodeHealthState{}, adopted)
	if !errors.Is(err, errAssignmentChanged) {
		t.Fatalf("error = %v, want assignment changed", err)
	}

	if got := inspections.Load(); got != 1 {
		t.Fatalf("CNI inspection calls = %d, want 1", got)
	}

	if _, statErr := os.Stat(cniConfigPath(cfg)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("active CNI config should remain disabled after failed inspection: %v", statErr)
	}

	after, readErr := os.ReadFile(cniDisabledPath(cfg))
	if readErr != nil {
		t.Fatal(readErr)
	}

	if string(after) != string(existing) {
		t.Fatalf("disabled CNI config changed during assignment retry: got %q, want %q", after, existing)
	}
}

func TestWaitForAdoptedNodePodCIDRsAndConfigureCancellationPreservesCNI(t *testing.T) {
	const nodeName = "node-a"

	node := assignedBootstrapNode(nodeName, "uid-a", "site-a", "", "10.244.1.0/24")
	cfg := adoptedBootstrapTestConfig(t, nodeName)
	existing := []byte(`{"cniVersion":"0.4.0","name":"existing","plugins":[]}`)

	if err := os.WriteFile(cniConfigPath(cfg), existing, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	adopted := nodeAssignment{site: "site-a", nodeUID: node.UID}

	_, err := waitForAdoptedNodePodCIDRsAndConfigure(ctx, fake.NewClientset(node), cfg, &nodeHealthState{}, adopted)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}

	after, readErr := os.ReadFile(cniConfigPath(cfg))
	if readErr != nil {
		t.Fatal(readErr)
	}

	if string(after) != string(existing) {
		t.Fatalf("CNI config changed during cancellation: got %q, want %q", after, existing)
	}
}

func TestGatewayAssignmentSkipsManagedCNI(t *testing.T) {
	assignment := nodeAssignment{site: "site-a", gatewayPool: "pool-a", nodeUID: types.UID("uid-a")}
	siteInformer := newTestInformer()
	manageCNI := false

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: assignment.site},
		Spec:       unboundedv1alpha3.SiteSpec{ManageCniPlugin: &manageCNI},
	}
	if err := siteInformer.GetStore().Add(toUnstructuredSiteWatch(t, site)); err != nil {
		t.Fatal(err)
	}

	if getManageCniPluginFromCRDs(siteInformer, assignment.site) {
		t.Fatal("gateway must skip managed CNI when its Site uses an external CNI")
	}
}

func TestGatewayAssignmentConfiguresManagedCNI(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "enabled"}[explicit], func(t *testing.T) {
			node := assignedBootstrapNode("gateway-a", "uid-a", "site-a", "pool-a", "10.244.1.0/24")
			assignment := nodeAssignment{site: "site-a", gatewayPool: "pool-a", nodeUID: node.UID}

			site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: assignment.site}}
			if explicit {
				site.Spec.ManageCniPlugin = &explicit
			}

			siteInformer := newTestInformer()
			if err := siteInformer.GetStore().Add(toUnstructuredSiteWatch(t, site)); err != nil {
				t.Fatal(err)
			}

			if !getManageCniPluginFromCRDs(siteInformer, assignment.site) {
				t.Fatal("gateway must configure managed CNI when enabled by its Site")
			}

			cfg := adoptedBootstrapTestConfig(t, node.Name)
			healthState := &nodeHealthState{}
			healthState.beginManagedCNI(cfg.BridgeName)

			podCIDRs, err := waitForAdoptedNodePodCIDRsAndConfigure(t.Context(), fake.NewClientset(node), cfg, healthState, assignment)
			if err != nil {
				t.Fatal(err)
			}

			if len(podCIDRs) != 1 || podCIDRs[0] != node.Spec.PodCIDRs[0] {
				t.Fatalf("podCIDRs = %v, want %v", podCIDRs, node.Spec.PodCIDRs)
			}

			if _, err := os.Stat(cniConfigPath(cfg)); err != nil {
				t.Fatalf("gateway CNI config was not written: %v", err)
			}
		})
	}
}

func assignedBootstrapNode(name string, uid types.UID, site, gatewayPool, podCIDR string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:            name,
		UID:             uid,
		ResourceVersion: "1",
		Labels: map[string]string{
			unboundedv1alpha3.MachineSiteLabelKey: site,
		},
	}}
	if gatewayPool != "" {
		node.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = gatewayPool
	}

	if podCIDR != "" {
		node.Spec.PodCIDRs = []string{podCIDR}
	}

	return node
}

func adoptedBootstrapTestConfig(t *testing.T, nodeName string) *config {
	t.Helper()

	return &config{
		NodeName:         nodeName,
		CNIConfDir:       t.TempDir(),
		CNIConfFile:      "10-unbounded.conflist",
		BridgeName:       "cbr0",
		MTU:              1400,
		cniRetryInterval: time.Millisecond,
		cniInspector:     allowAllCNIInspection,
	}
}
