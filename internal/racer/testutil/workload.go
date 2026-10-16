// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides workload fixtures for controller tests.
package testutil

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

type Config = workload.Config

const (
	DataplaneDaemonSetName  = workload.DataplaneDaemonSetName
	PodNetworkDaemonSetName = workload.PodNetworkDaemonSetName
)

var (
	ConfigFromLookup  = workload.ConfigFromLookup
	DesiredDaemonSets = workload.DesiredDaemonSets
	ManagedNames      = workload.ManagedNames
)

// DesiredDaemonSet returns the single host-network workload planned for c.
// It rejects mixed networking, which plans two workloads.
func DesiredDaemonSet(c Config) (*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	if len(c.PodNetworkNodes) != 0 {
		return nil, fmt.Errorf("mixed networking plans two workloads: %w", wire.InvalidRequest)
	}

	sets, err := workload.DesiredDaemonSets(c)
	if err != nil {
		return nil, err
	}

	return sets[0], nil
}
