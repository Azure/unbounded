// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MachineRefFieldSelector selects the MachineOperations whose spec.machineRef
// is machineName.
func MachineRefFieldSelector(machineName string) fields.Selector {
	return fields.OneTermEqualSelector(machinav1alpha3.MachineOperationMachineRefField, machineName)
}

// MachineRefSelectable reports whether the API server accepts spec.machineRef
// as a field selector on MachineOperation list and watch. An API server from
// Kubernetes 1.31 on, with a CRD that declares the field selectable, accepts
// it. An older one, or an older CRD, rejects it with BadRequest.
//
// The agent calls this once at startup with an uncached reader. When the
// answer is true, its MachineOperation cache and every live list are filtered
// by spec.machineRef. The field selector is what keeps the traffic to one
// agent independent of the size of the fleet: the API server sends each agent
// only the operations that name its machine.
func MachineRefSelectable(ctx context.Context, reader client.Reader, machineName string) (bool, error) {
	var ops machinav1alpha3.MachineOperationList

	err := reader.List(
		ctx, &ops,
		client.MatchingFieldsSelector{Selector: MachineRefFieldSelector(machineName)},
		client.Limit(1),
	)
	if err != nil {
		if apierrors.IsBadRequest(err) {
			return false, nil
		}

		return false, fmt.Errorf("probe MachineOperation %s selection: %w", machinav1alpha3.MachineOperationMachineRefField, err)
	}

	return true, nil
}
