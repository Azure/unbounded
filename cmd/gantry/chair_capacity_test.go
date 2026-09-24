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

func TestBoundedChairHolderTarget(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		maximum  int
		want     int
	}{
		{name: "small cluster", capacity: 2, maximum: 64, want: 2},
		{name: "below maximum", capacity: 32, maximum: 64, want: 32},
		{name: "at maximum", capacity: 64, maximum: 64, want: 64},
		{name: "above maximum", capacity: 1000, maximum: 64, want: 64},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := boundedChairHolderTarget(test.capacity, test.maximum); got != test.want {
				t.Fatalf("target = %d; want %d", got, test.want)
			}
		})
	}
}

func TestDaemonSetChairCapacityHolderTarget(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry", Namespace: "gantry-system"},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 21},
	})
	source := daemonSetChairCapacity{
		daemonSets: client.AppsV1().DaemonSets("gantry-system"),
		name:       "gantry",
		maximum:    64,
	}

	target, err := source.HolderTarget(context.Background())
	if err != nil {
		t.Fatalf("HolderTarget: %v", err)
	}

	if target != 21 {
		t.Fatalf("target = %d; want 21", target)
	}
}

func TestDaemonSetChairCapacityRejectsZeroDesiredCapacity(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry", Namespace: "gantry-system"},
	})
	source := daemonSetChairCapacity{
		daemonSets: client.AppsV1().DaemonSets("gantry-system"),
		name:       "gantry",
		maximum:    64,
	}

	if _, err := source.HolderTarget(context.Background()); err == nil {
		t.Fatal("HolderTarget succeeded with zero desired capacity")
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
