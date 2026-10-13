// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/samber/lo"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"

	karpenterv1alpha1 "github.com/Azure/unbounded/api/karpenter/v1alpha1"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/karpenterprovider"
	"github.com/Azure/unbounded/internal/unbounded"
)

func init() {
	utilruntime.Must(machinav1alpha3.AddToScheme(clientgoscheme.Scheme))
	utilruntime.Must(karpenterv1alpha1.AddToScheme(clientgoscheme.Scheme))
}

func main() {
	operator.AppName = "unbounded-karpenter-provider"
	ctx, op := operator.NewOperator()

	holder, err := os.Hostname()
	if err != nil {
		panic(fmt.Errorf("resolve lease holder hostname: %w", err))
	}

	holder = fmt.Sprintf("%s-%d", holder, os.Getpid())

	undecorated := karpenterprovider.New(
		op.GetClient(),
		&karpenterprovider.KubernetesDomainLocker{
			Client:         op.GetClient(),
			Namespace:      unbounded.SystemNamespace(),
			HolderIdentity: holder,
		},
		karpenterprovider.UnavailableLifecycle{},
	)
	provider := overlay.Decorate(undecorated, op.GetClient(), op.InstanceTypeStore)
	repairPolicyMatcher := lo.Must(health.NewRepairPolicyMatcher(ctx, provider))
	clusterState := state.NewCluster(op.Clock, op.GetClient(), provider, state.WithRepairPolicyMatcher(repairPolicyMatcher))

	lo.Must0((&karpenterprovider.NodeClassReconciler{Client: op.GetClient()}).SetupWithManager(op.Manager))
	op.WithControllers(ctx, controllers.NewControllers(
		ctx,
		op.Manager,
		op.Clock,
		op.GetClient(),
		op.EventRecorder,
		provider,
		undecorated,
		clusterState,
		op.InstanceTypeStore,
		op.PredictionStore,
	)...).Start(ctx)
}
