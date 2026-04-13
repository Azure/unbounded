// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cloudprovider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDetectProvider_AKS(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "aks-cluster-metadata",
				Namespace: metav1.NamespacePublic,
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "aks-system-0",
				Labels: map[string]string{
					"kubernetes.azure.com/mode":    "system",
					"kubernetes.azure.com/cluster": "mc_rg_my-cluster_eastus",
				},
			},
		},
	)

	provider, err := DetectProvider(context.Background(), kubeCli)
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, "microsoft-aks", provider.ID())

	labels := provider.DefaultLabels()
	require.Equal(t, "false", labels["kubernetes.azure.com/managed"])
	require.Equal(t, "mc_rg_my-cluster_eastus", labels["kubernetes.azure.com/cluster"])
}

func TestDetectProvider_NotAKS(t *testing.T) {
	t.Parallel()

	// No aks-cluster-metadata ConfigMap — vanilla cluster.
	kubeCli := fake.NewClientset()

	provider, err := DetectProvider(context.Background(), kubeCli)
	require.NoError(t, err)
	require.Nil(t, provider)
}

func TestDetectProvider_AKS_NoSystemNodes(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "aks-cluster-metadata",
				Namespace: metav1.NamespacePublic,
			},
		},
		// No nodes with kubernetes.azure.com/mode=system.
	)

	provider, err := DetectProvider(context.Background(), kubeCli)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no nodes found")
	require.Nil(t, provider)
}

func TestDetectProvider_AKS_MissingClusterLabel(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "aks-cluster-metadata",
				Namespace: metav1.NamespacePublic,
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "aks-system-0",
				Labels: map[string]string{
					"kubernetes.azure.com/mode": "system",
					// Missing kubernetes.azure.com/cluster label.
				},
			},
		},
	)

	provider, err := DetectProvider(context.Background(), kubeCli)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing kubernetes.azure.com/cluster label")
	require.Nil(t, provider)
}

func TestAKSProvider_DefaultLabels(t *testing.T) {
	t.Parallel()

	p := &AKSProvider{ClusterName: "mc_rg_test_eastus"}
	require.Equal(t, "microsoft-aks", p.ID())

	labels := p.DefaultLabels()

	require.Len(t, labels, 2)
	require.Equal(t, "false", labels["kubernetes.azure.com/managed"])
	require.Equal(t, "mc_rg_test_eastus", labels["kubernetes.azure.com/cluster"])
}

func TestCommonDefaultLabels(t *testing.T) {
	t.Parallel()

	labels := CommonDefaultLabels()

	require.Empty(t, labels)
}

func TestIsKubeletAllowedLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		allowed bool
	}{
		// Labels without a domain are always allowed.
		{name: "no-domain", key: "my-label", allowed: true},

		// Labels with non-kubernetes.io domains are always allowed.
		{name: "custom-domain", key: "example.com/label", allowed: true},
		{name: "azure-domain", key: "kubernetes.azure.com/managed", allowed: true},

		// Allowed kubernetes.io prefixes.
		{name: "kubelet-prefix", key: "kubelet.kubernetes.io/os", allowed: true},
		{name: "node-prefix", key: "node.kubernetes.io/instance-type", allowed: true},

		// Allowed exact labels.
		{name: "kubernetes-io-arch", key: "kubernetes.io/arch", allowed: true},
		{name: "kubernetes-io-hostname", key: "kubernetes.io/hostname", allowed: true},
		{name: "kubernetes-io-os", key: "kubernetes.io/os", allowed: true},
		{name: "topology-region", key: "topology.kubernetes.io/region", allowed: true},
		{name: "topology-zone", key: "topology.kubernetes.io/zone", allowed: true},
		{name: "beta-arch", key: "beta.kubernetes.io/arch", allowed: true},
		{name: "beta-os", key: "beta.kubernetes.io/os", allowed: true},
		{name: "beta-instance-type", key: "beta.kubernetes.io/instance-type", allowed: true},
		{name: "failure-domain-region", key: "failure-domain.beta.kubernetes.io/region", allowed: true},
		{name: "failure-domain-zone", key: "failure-domain.beta.kubernetes.io/zone", allowed: true},

		// Restricted kubernetes.io labels.
		{name: "cloud-provider-exclude", key: "node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider", allowed: false},
		{name: "arbitrary-kubernetes-io", key: "foo.kubernetes.io/bar", allowed: false},
		{name: "arbitrary-k8s-io", key: "foo.k8s.io/bar", allowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.allowed, IsKubeletAllowedLabel(tt.key))
		})
	}
}

func TestPartitionNodeLabels(t *testing.T) {
	t.Parallel()

	input := map[string]string{
		"my-custom-label":              "value1",
		"kubernetes.azure.com/managed": "false",
		"kubernetes.io/hostname":       "my-node",
		"node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider": "true",
		"node.kubernetes.io/custom":      "value2",
		"restricted.kubernetes.io/label": "value3",
	}

	kubeletLabels, controllerLabels := PartitionNodeLabels(input)

	// Kubelet-allowed labels.
	require.Equal(t, "value1", kubeletLabels["my-custom-label"])
	require.Equal(t, "false", kubeletLabels["kubernetes.azure.com/managed"])
	require.Equal(t, "my-node", kubeletLabels["kubernetes.io/hostname"])
	require.Equal(t, "value2", kubeletLabels["node.kubernetes.io/custom"])
	require.Len(t, kubeletLabels, 4)

	// Controller-managed labels.
	require.Equal(t, "true", controllerLabels["node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider"])
	require.Equal(t, "value3", controllerLabels["restricted.kubernetes.io/label"])
	require.Len(t, controllerLabels, 2)
}

func TestPartitionNodeLabels_Empty(t *testing.T) {
	t.Parallel()

	kubeletLabels, controllerLabels := PartitionNodeLabels(map[string]string{})
	require.Empty(t, kubeletLabels)
	require.Empty(t, controllerLabels)
}

func TestPartitionNodeLabels_AllAllowed(t *testing.T) {
	t.Parallel()

	input := map[string]string{
		"custom-label":              "v1",
		"kubernetes.io/hostname":    "node1",
		"kubernetes.azure.com/mode": "user",
	}

	kubeletLabels, controllerLabels := PartitionNodeLabels(input)
	require.Len(t, kubeletLabels, 3)
	require.Empty(t, controllerLabels)
}

func TestPartitionNodeLabels_AllRestricted(t *testing.T) {
	t.Parallel()

	input := map[string]string{
		"node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider": "true",
		"restricted.kubernetes.io/other":                                        "val",
	}

	kubeletLabels, controllerLabels := PartitionNodeLabels(input)
	require.Empty(t, kubeletLabels)
	require.Len(t, controllerLabels, 2)
}
