// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package karpenterprovider

import (
	"context"
	"errors"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

var ErrLifecycleUnavailable = errors.New("worker lifecycle operation is unavailable")

// Lifecycle is the narrow southbound boundary for provisioning and release.
// Karpenter completes drain and PDB handling before CloudProvider.Delete.
type Lifecycle interface {
	ProvisionWorker(context.Context, *machinav1alpha3.Machine) error
	RevokeCredentials(context.Context, *machinav1alpha3.Machine) error
	ResetWorker(context.Context, *machinav1alpha3.Machine) error
	DeleteNode(context.Context, *machinav1alpha3.Machine) error
}

type UnavailableLifecycle struct{}

func (UnavailableLifecycle) ProvisionWorker(context.Context, *machinav1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableLifecycle) RevokeCredentials(context.Context, *machinav1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableLifecycle) ResetWorker(context.Context, *machinav1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}

func (UnavailableLifecycle) DeleteNode(context.Context, *machinav1alpha3.Machine) error {
	return ErrLifecycleUnavailable
}
