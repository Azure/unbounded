// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveClusterInfoDiscoversAPIServerFromClusterInfo(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewSimpleClientset(
		clusterInfoConfigMap("https://api.example.com:6443", []byte("ca-from-cluster-info")),
		kubeDNSService(),
	)

	info, err := ResolveClusterInfo(context.Background(), "", kubeCli)
	require.NoError(t, err)
	require.Equal(t, "api.example.com:6443", info.APIServer)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("ca-from-cluster-info")), info.CACertBase64)
	require.Equal(t, "10.0.0.10", info.ClusterDNS)
}

func TestResolveClusterInfoUsesExplicitAPIServerEndpoint(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespacePublic},
			Data:       map[string]string{"ca.crt": "ca-from-root"},
		},
		kubeDNSService(),
	)

	info, err := ResolveClusterInfo(context.Background(), "https://override.example.com:6443", kubeCli)
	require.NoError(t, err)
	require.Equal(t, "override.example.com:6443", info.APIServer)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("ca-from-root")), info.CACertBase64)
}

func TestResolveClusterInfoFailsWithoutDiscoverableAPIServer(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewSimpleClientset(kubeDNSService())

	_, err := ResolveClusterInfo(context.Background(), "", kubeCli)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get cluster-info ConfigMap")
}

func clusterInfoConfigMap(server string, ca []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: metav1.NamespacePublic},
		Data: map[string]string{
			"kubeconfig": `apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: ` + server + `
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString(ca) + `
contexts:
- name: context
  context:
    cluster: cluster
current-context: context
`,
		},
	}
}

func kubeDNSService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: metav1.NamespaceSystem},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.10"},
	}
}
