// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func ClientAndConfigFromBytes(b []byte) (*kubernetes.Clientset, *rest.Config, error) {
	cliCfg, err := clientcmd.NewClientConfigFromBytes(b)
	if err != nil {
		return nil, nil, fmt.Errorf("new kubernetes client config: %w", err)
	}

	restCfg, err := cliCfg.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("new kubernetes rest client config: %w", err)
	}

	cli, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("new kubernetes client: %w", err)
	}

	return cli, restCfg, nil
}
