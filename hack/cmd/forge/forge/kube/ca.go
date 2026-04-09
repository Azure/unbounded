// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func GetRootKubernetesCA(k kubernetes.Interface) ([]byte, error) {
	// Get the kube-root-ca.crt ConfigMap from kube-public namespace
	// This ConfigMap is automatically created by Kubernetes and contains the cluster CA certificate
	cm, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(context.Background(), "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-root-ca.crt ConfigMap: %w", err)
	}

	// Extract the ca.crt data
	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("ca.crt not found in kube-root-ca.crt ConfigMap")
	}

	return []byte(caCert), nil
}
