// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DetectProviderLabels returns the labels the cluster's provider requires on
// every node, for BuildAgentConfigParams.ProviderLabels. It returns nil when
// the provider is unknown (e.g. vanilla Kubernetes, on-prem, k3s). An error
// means detection failed, not that the provider is unknown.
//
// The only known provider is AKS, detected by the aks-cluster-metadata
// ConfigMap in kube-public.
func DetectProviderLabels(ctx context.Context, k kubernetes.Interface) (map[string]string, error) {
	_, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "aks-cluster-metadata", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get aks-cluster-metadata ConfigMap: %w", err)
	}

	return map[string]string{"kubernetes.azure.com/managed": "false"}, nil
}
