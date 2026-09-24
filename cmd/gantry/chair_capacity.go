// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
)

func boundedChairHolderTarget(capacity, maximum int) int {
	if capacity > maximum {
		return maximum
	}

	return capacity
}

type daemonSetChairCapacity struct {
	daemonSets appsv1.DaemonSetInterface
	name       string
	maximum    int
}

func (c daemonSetChairCapacity) HolderTarget(ctx context.Context) (int, error) {
	daemonSet, err := c.daemonSets.Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get Gantry DaemonSet capacity: %w", err)
	}

	capacity := int(daemonSet.Status.DesiredNumberScheduled)
	if capacity < 1 {
		return 0, fmt.Errorf("gantry daemonset desired capacity is %d", capacity)
	}

	return boundedChairHolderTarget(capacity, c.maximum), nil
}

func chairDHTReady(clusterSizeEstimate, routingTableSize int, holdingChair bool) bool {
	return clusterSizeEstimate <= 1 || routingTableSize > 0 || holdingChair
}
