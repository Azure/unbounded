// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes client, preferring in-cluster credentials and
// falling back to an explicit kubeconfig path.
//
// racer-ctrl runs as a DaemonSet so in-cluster is the normal path; the
// kubeconfig fallback exists so the agent can be run against a cluster from a
// developer's machine while pointing at a fake config directory.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	// The agent's request pattern is a handful of watches plus an occasional
	// annotation patch, but a node that has just come up patches once per
	// staged volume in quick succession. Lifting the client-side limits off
	// the defaults keeps that burst from being self-throttled into the
	// kubelet's stage timeout.
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.UserAgent = "racer-ctrl"

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	return client, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
		}

		return cfg, nil
	}

	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("load in-cluster config: %w", err)
	}

	return nil, errors.New("not running in a cluster and no KUBECONFIG was supplied")
}
