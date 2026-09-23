// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProportionalChairSeedTarget(t *testing.T) {
	tests := []struct {
		name       string
		capacity   int
		percentage int
		maximum    int
		want       int
	}{
		{name: "two nodes", capacity: 2, percentage: 10, maximum: 50, want: 1},
		{name: "three nodes", capacity: 3, percentage: 10, maximum: 50, want: 1},
		{name: "twenty nodes", capacity: 20, percentage: 10, maximum: 50, want: 2},
		{name: "round up", capacity: 21, percentage: 10, maximum: 50, want: 3},
		{name: "maximum", capacity: 1000, percentage: 10, maximum: 50, want: 50},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := proportionalChairSeedTarget(test.capacity, test.percentage, test.maximum); got != test.want {
				t.Fatalf("target = %d; want %d", got, test.want)
			}
		})
	}
}

func TestDaemonSetChairCapacitySeedTarget(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry", Namespace: "gantry-system"},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 21},
	})
	source := daemonSetChairCapacity{
		daemonSets: client.AppsV1().DaemonSets("gantry-system"),
		name:       "gantry",
		percentage: 10,
		maximum:    50,
	}

	target, err := source.SeedTarget(context.Background())
	if err != nil {
		t.Fatalf("SeedTarget: %v", err)
	}

	if target != 3 {
		t.Fatalf("target = %d; want 3", target)
	}
}

func TestDaemonSetChairCapacityRejectsZeroDesiredCapacity(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry", Namespace: "gantry-system"},
	})
	source := daemonSetChairCapacity{
		daemonSets: client.AppsV1().DaemonSets("gantry-system"),
		name:       "gantry",
		percentage: 10,
		maximum:    50,
	}

	if _, err := source.SeedTarget(context.Background()); err == nil {
		t.Fatal("SeedTarget succeeded with zero desired capacity")
	}
}

func TestChairDHTReady(t *testing.T) {
	tests := []struct {
		name             string
		clusterEstimate  int
		routingTableSize int
		holdingChair     bool
		want             bool
	}{
		{name: "connected non-chair", clusterEstimate: 100, routingTableSize: 1, want: true},
		{name: "isolated chair", clusterEstimate: 100, holdingChair: true, want: true},
		{name: "isolated non-chair", clusterEstimate: 100, want: false},
		{name: "single node", clusterEstimate: 1, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := chairDHTReady(test.clusterEstimate, test.routingTableSize, test.holdingChair); got != test.want {
				t.Fatalf("ready = %t; want %t", got, test.want)
			}
		})
	}
}
