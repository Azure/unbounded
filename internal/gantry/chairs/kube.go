// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs

import (
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func NewClientset(kubeconfig string) (kubernetes.Interface, error) {
	var (
		config *rest.Config
		err    error
	)

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}

	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			return nil, errors.New("chairs: not in cluster and no kubeconfig supplied")
		}

		return nil, fmt.Errorf("chairs: load Kubernetes config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("chairs: build Kubernetes client: %w", err)
	}

	return client, nil
}
