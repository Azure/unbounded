// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package controller wires the provider into the pinned upstream Karpenter controllers.
package controller

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"
)

// Controllers returns the pinned upstream controllers driven by provider,
// followed by extra, the plugin's own controllers.
func Controllers(
	ctx context.Context,
	operator *operator.Operator,
	provider cloudprovider.CloudProvider,
	extra ...controller.Controller,
) []controller.Controller {
	decorated := overlay.Decorate(provider, operator.GetClient(), operator.InstanceTypeStore)
	cluster := state.NewCluster(operator.Clock, operator.GetClient(), decorated)

	return append(controllers.NewControllers(
		ctx,
		operator.Manager,
		operator.Clock,
		operator.GetClient(),
		operator.EventRecorder,
		decorated,
		provider,
		cluster,
		operator.InstanceTypeStore,
		operator.PredictionStore,
	), extra...)
}
