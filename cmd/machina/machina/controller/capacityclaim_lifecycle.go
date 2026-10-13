// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

var ErrLifecycleUnavailable = errors.New("capacity lifecycle operation is unavailable")

// CapacityLifecycle isolates destructive release operations from allocation
// state management.
type CapacityLifecycle interface {
	ProvisionWorker(ctx context.Context, machine *unboundedv1alpha3.Machine) error
	CordonDrain(ctx context.Context, machine *unboundedv1alpha3.Machine) error
	RevokeCredentials(ctx context.Context, machine *unboundedv1alpha3.Machine) error
	ResetWorker(ctx context.Context, machine *unboundedv1alpha3.Machine) error
	DeleteNode(ctx context.Context, machine *unboundedv1alpha3.Machine) error
}

// UnavailableCapacityLifecycle is the production default until southbound
// implementations are explicitly wired. It fails closed.
type UnavailableCapacityLifecycle struct{}

func (UnavailableCapacityLifecycle) ProvisionWorker(context.Context, *unboundedv1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableCapacityLifecycle) CordonDrain(context.Context, *unboundedv1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableCapacityLifecycle) RevokeCredentials(context.Context, *unboundedv1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableCapacityLifecycle) ResetWorker(context.Context, *unboundedv1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableCapacityLifecycle) DeleteNode(context.Context, *unboundedv1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}
