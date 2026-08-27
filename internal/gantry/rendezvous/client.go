// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"fmt"

	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewLeaseClient builds a namespace-scoped Lease client from in-cluster
// credentials or an explicit kubeconfig.
func NewLeaseClient(kubeconfig, namespace string) (coordinationclient.LeaseInterface, error) {
	var (
		config *rest.Config
		err    error
	)
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if err != nil {
		return nil, fmt.Errorf("rendezvous: load Kubernetes config: %w", err)
	}

	client, err := coordinationclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("rendezvous: build Lease client: %w", err)
	}

	return client.Leases(namespace), nil
}
