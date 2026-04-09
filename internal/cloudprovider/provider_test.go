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

	require.Contains(t, labels, ExcludeFromCloudProviderLabel)
	require.Equal(t, "true", labels[ExcludeFromCloudProviderLabel])
}

func TestExcludeFromCloudProviderLabel_Constant(t *testing.T) {
	t.Parallel()

	require.Equal(t, "node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider", ExcludeFromCloudProviderLabel)
}
