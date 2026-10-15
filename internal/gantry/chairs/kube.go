// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

	// Kubernetes is an external API, not an owned Gantry peer endpoint. Keep
	// client-go's TLS/auth/proxy policy, including kubeconfig transport settings.
	// The owned chair HTTPS transport separately enforces TLS 1.3.
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("chairs: build Kubernetes client: %w", err)
	}

	return client, nil
}
