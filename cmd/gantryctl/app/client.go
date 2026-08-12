// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type clusterClients struct {
	kube      kubernetes.Interface
	resources client.Client
	rest      *rest.Config
}

func (o *rootOptions) clusterClients() (*clusterClients, error) {
	if o.clients != nil {
		return o.clients, nil
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.kubeconfig != "" {
		rules.ExplicitPath = o.kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.context}
	deferred := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	restConfig, err := deferred.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	resourceClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes resource client: %w", err)
	}

	o.clients = &clusterClients{kube: kubeClient, resources: resourceClient, rest: restConfig}

	return o.clients, nil
}
