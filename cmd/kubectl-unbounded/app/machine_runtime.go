// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type machineClientFactory func(kubeconfig string) (client.WithWatch, error)

type machineCommandRuntime struct {
	newClientWithKubeconfig machineClientFactory
	commandContext          func(context.Context) context.Context
}

func newMachineCommandRuntime() *machineCommandRuntime {
	return &machineCommandRuntime{
		newClientWithKubeconfig: newMachineClientWithKubeconfig,
		commandContext: func(context.Context) context.Context {
			return ctrl.SetupSignalHandler()
		},
	}
}

func (rt *machineCommandRuntime) clientWithKubeconfig(kubeconfig string) (client.WithWatch, error) {
	if rt == nil || rt.newClientWithKubeconfig == nil {
		return newMachineClientWithKubeconfig(kubeconfig)
	}

	return rt.newClientWithKubeconfig(kubeconfig)
}

func (rt *machineCommandRuntime) context(ctx context.Context) context.Context {
	if rt == nil || rt.commandContext == nil {
		return ctx
	}

	return rt.commandContext(ctx)
}
