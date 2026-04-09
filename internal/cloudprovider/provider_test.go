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
	)

	provider, err := DetectProvider(context.Background(), kubeCli)
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, "microsoft-aks", provider.ID())

	labels := provider.DefaultLabels()
	require.Equal(t, "false", labels["kubernetes.azure.com/managed"])
	require.NotContains(t, labels, "kubernetes.azure.com/cluster")
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

	// AKS detection no longer requires system-mode nodes; only the
	// aks-cluster-metadata ConfigMap is needed.
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
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, "microsoft-aks", provider.ID())
}

func TestDetectProvider_AKS_MissingClusterLabel(t *testing.T) {
	t.Parallel()

	// AKS detection no longer reads the kubernetes.azure.com/cluster label,
	// so a node missing that label does not cause an error.
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
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, "microsoft-aks", provider.ID())
}

func TestAKSProvider_DefaultLabels(t *testing.T) {
	t.Parallel()

	p := &AKSProvider{}
	require.Equal(t, "microsoft-aks", p.ID())

	labels := p.DefaultLabels()

	require.Len(t, labels, 1)
	require.Equal(t, "false", labels["kubernetes.azure.com/managed"])
	require.NotContains(t, labels, "kubernetes.azure.com/cluster")
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
