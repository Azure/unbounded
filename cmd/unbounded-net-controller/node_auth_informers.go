// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded/internal/net/authn"
)

// These watches run on every serving replica, independently of leadership.
// HasSynced is an initial sync barrier, not proof of ongoing watch freshness.
type nodeAuthInformers struct {
	ctx             context.Context
	namespace       string
	podFactory      informers.SharedInformerFactory
	saFactory       informers.SharedInformerFactory
	pods            coreinformers.PodInformer
	serviceAccounts coreinformers.ServiceAccountInformer
}

func newNodeAuthInformers(ctx context.Context, client kubernetes.Interface, namespace string, resync time.Duration) *nodeAuthInformers {
	pods := informers.NewSharedInformerFactoryWithOptions(client, resync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = "app.kubernetes.io/name=unbounded-net-node"
		}),
	)
	serviceAccounts := informers.NewSharedInformerFactoryWithOptions(client, resync, informers.WithNamespace(namespace))
	result := &nodeAuthInformers{
		ctx: ctx, namespace: namespace,
		podFactory: pods, saFactory: serviceAccounts,
		pods: pods.Core().V1().Pods(), serviceAccounts: serviceAccounts.Core().V1().ServiceAccounts(),
	}
	// Instantiate both informers before starting their factories.
	result.pods.Informer()
	result.serviceAccounts.Informer()

	return result
}

func (i *nodeAuthInformers) start() {
	i.podFactory.Start(i.ctx.Done())
	i.saFactory.Start(i.ctx.Done())
}

func (i *nodeAuthInformers) ready() bool {
	return i.ctx.Err() == nil &&
		!i.pods.Informer().IsStopped() && !i.serviceAccounts.Informer().IsStopped() &&
		i.pods.Informer().HasSynced() && i.serviceAccounts.Informer().HasSynced()
}

func (i *nodeAuthInformers) wrapOIDCFactory(factory oidcVerifierFactory) oidcVerifierFactory {
	return func(ctx context.Context, issuer, audience string) (serviceAccountTokenVerifier, error) {
		verifier, err := factory(ctx, issuer, audience)
		if err != nil {
			return nil, err
		}

		return authn.NewPodBoundTokenVerifier(verifier, authn.PodBoundTokenVerifierOptions{
			Namespace: i.namespace, ServiceAccount: "unbounded-net-node",
			Pods: i.pods.Lister(), ServiceAccounts: i.serviceAccounts.Lister(), Ready: i.ready,
		}), nil
	}
}
