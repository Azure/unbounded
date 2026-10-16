// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
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
