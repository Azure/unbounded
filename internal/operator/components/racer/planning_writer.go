// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Recover may finish staged initialization, but the operator's Plan is read-only.
type planningWriter struct{}

var errPlanningWrite = errors.New("racer initialization requires controller recovery, not operator planning")

func (planningWriter) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errPlanningWrite
}

func (planningWriter) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errPlanningWrite
}

func (planningWriter) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return errPlanningWrite
}

func (planningWriter) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errPlanningWrite
}

func (planningWriter) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	return errPlanningWrite
}

func (planningWriter) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return errPlanningWrite
}
