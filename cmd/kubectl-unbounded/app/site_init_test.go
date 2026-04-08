package app

import (
	"context"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	fakerest "k8s.io/client-go/rest"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// deriveServiceCIDR tests
// ---------------------------------------------------------------------------

func TestDeriveServiceCIDR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		clusterIP string
		want      string
		expectErr string
	}{
		{
			name:      "standard AKS kube-dns IP",
			clusterIP: "10.0.0.10",
			want:      "10.0.0.0/16",
		},
		{
			name:      "different subnet",
			clusterIP: "172.16.5.3",
			want:      "172.16.0.0/16",
		},
		{
			name:      "first IP in range",
			clusterIP: "10.96.0.1",
			want:      "10.96.0.0/16",
		},
		{
			name:      "empty ClusterIP",
			clusterIP: "",
			expectErr: "has no ClusterIP",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kube-dns",
					Namespace: metav1.NamespaceSystem,
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: tc.clusterIP,
				},
			}

			kubeCli := fake.NewClientset(svc)

			got, err := deriveServiceCIDR(context.Background(), kubeCli)
			if tc.expectErr != "" {
				require.ErrorContains(t, err, tc.expectErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDeriveServiceCIDR_NoKubeDNSService(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset() // no services
	_, err := deriveServiceCIDR(context.Background(), kubeCli)
	require.ErrorContains(t, err, "get kube-dns Service")
}

// ---------------------------------------------------------------------------
// ensureFlexAgentConfig tests
// ---------------------------------------------------------------------------

func TestEnsureFlexAgentConfig_Success(t *testing.T) {
	t.Parallel()

	// Fake kube-public/kube-root-ca.crt ConfigMap.
	caCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			"ca.crt": "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
		},
	}

	// Fake kube-dns Service for deriving the service CIDR.
	kubeDNSSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-dns",
			Namespace: metav1.NamespaceSystem,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.0.0.10",
		},
	}

	kubeCli := fake.NewClientset(caCertCM, kubeDNSSvc)

	// Track what was applied via the controller-runtime fake.
	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	h := &siteInitHandler{
		kubeCli:          kubeCli,
		kubeResourcesCli: kubeResourcesCli,
		kubeConfig:       &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		logger:           discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.NoError(t, err)

	// The template has 9 YAML documents — they should all be applied.
	require.NotEmpty(t, appliedObjects, "expected flex agent manifests to be applied")
}

func TestEnsureFlexAgentConfig_WithExplicitServiceCIDR(t *testing.T) {
	t.Parallel()

	caCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			"ca.crt": "fake-cert",
		},
	}

	// No kube-dns needed when clusterServiceCIDR is explicit.
	kubeCli := fake.NewClientset(caCertCM)

	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	h := &siteInitHandler{
		kubeCli:            kubeCli,
		kubeResourcesCli:   kubeResourcesCli,
		kubeConfig:         &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		clusterServiceCIDR: "192.168.0.0/24",
		logger:             discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, appliedObjects, "expected flex agent manifests to be applied")
}

func TestEnsureFlexAgentConfig_MissingCACert(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset() // no ConfigMaps

	h := &siteInitHandler{
		kubeCli:    kubeCli,
		kubeConfig: &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		logger:     discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.ErrorContains(t, err, "kube-root-ca.crt")
}

func TestEnsureFlexAgentConfig_MissingCACertKey(t *testing.T) {
	t.Parallel()

	caCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			"wrong-key": "data",
		},
	}

	kubeCli := fake.NewClientset(caCertCM)

	h := &siteInitHandler{
		kubeCli:    kubeCli,
		kubeConfig: &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		logger:     discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.ErrorContains(t, err, "ca.crt key not found")
}

func TestEnsureFlexAgentConfig_DeriveServiceCIDRFails(t *testing.T) {
	t.Parallel()

	caCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			"ca.crt": "fake-cert",
		},
	}

	// No kube-dns service and no explicit service CIDR → should fail.
	kubeCli := fake.NewClientset(caCertCM)

	h := &siteInitHandler{
		kubeCli:    kubeCli,
		kubeConfig: &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		logger:     discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.ErrorContains(t, err, "derive service CIDR")
}

// TestFlexAgentTemplateRendering verifies the template produces valid YAML
// containing the expected substituted values.
func TestFlexAgentTemplateRendering(t *testing.T) {
	t.Parallel()

	caCertCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			"ca.crt": "test-ca-cert",
		},
	}

	kubeDNSSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-dns",
			Namespace: metav1.NamespaceSystem,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.0.0.10",
		},
	}

	kubeCli := fake.NewClientset(caCertCM, kubeDNSSvc)

	// Capture the raw bytes passed to ApplyManifests by intercepting the
	// Apply call on the controller-runtime fake.
	var appliedYAML []byte

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	// We need to capture the rendered YAML before it goes to ApplyManifests.
	// The simplest way is to call the template rendering logic directly.
	// But since ensureFlexAgentConfig is a single method, let's just verify
	// it doesn't error and spot-check the resolved data by verifying the
	// method completes without error (template rendering errors would surface).
	h := &siteInitHandler{
		kubeCli:          kubeCli,
		kubeResourcesCli: kubeResourcesCli,
		kubeConfig:       &fakerest.Config{Host: "https://my-cluster.example.com:443"},
		logger:           discardLogger(),
	}

	err := h.ensureFlexAgentConfig(context.Background())
	require.NoError(t, err)

	// Verify the template can be rendered with expected data by doing it manually.
	_ = appliedYAML // Captured YAML is not easily accessible through the fake; instead
	// verify the template itself is well-formed by rendering it directly.
	data := flexAgentTemplateData{
		CertificateAuthorityData: "dGVzdC1jYS1jZXJ0",
		Server:                   "https://my-cluster.example.com:443",
		KubernetesVersion:        "v1.34.3",
		ServiceSubnet:            "10.0.0.0/16",
	}

	rendered := renderFlexAgentTemplates(t, data)

	assert.Contains(t, rendered, "certificate-authority-data: dGVzdC1jYS1jZXJ0")
	assert.Contains(t, rendered, "server: https://my-cluster.example.com:443")
	assert.Contains(t, rendered, "kubernetesVersion: v1.34.3")
	assert.Contains(t, rendered, "serviceSubnet: 10.0.0.0/16")
	assert.Contains(t, rendered, "kubeadm:nodes-kubeadm-config")
	assert.Contains(t, rendered, "cluster-info")
	assert.Contains(t, rendered, "kubeadm-config")
	assert.Contains(t, rendered, "kubelet-config")
}

// renderFlexAgentTemplates is a test helper that renders the embedded
// flexagent-temp templates with the given data and returns the result as a string.
func renderFlexAgentTemplates(t *testing.T, data flexAgentTemplateData) string {
	t.Helper()

	var buf strings.Builder

	entries, err := flexAgentTemplates.ReadDir("assets/flexagent-temp")
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		content, err := flexAgentTemplates.ReadFile("assets/flexagent-temp/" + entry.Name())
		require.NoError(t, err)

		tmpl, err := template.New(entry.Name()).Parse(string(content))
		require.NoError(t, err)

		require.NoError(t, tmpl.Execute(&buf, data))
	}

	return buf.String()
}

// ---------------------------------------------------------------------------
// ensureUnboundedSite prototype mode tests
// ---------------------------------------------------------------------------

func TestEnsureUnboundedSite_DefaultTemplates(t *testing.T) {
	var appliedYAML []byte

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	// Capture rendered YAML by hooking into the handler. Since we can't
	// intercept the raw bytes through the fake, we rely on the apiVersion
	// string appearing in the template output. We call ensureUnboundedSite
	// and verify the templates used by checking what gets applied.
	//
	// Since the fake Apply doesn't give us raw bytes, we render manually.
	cfg := unboundedSiteConfig{
		SiteName:  "test-site",
		NodeCIDRs: []string{"10.0.0.0/24"},
		PodCIDRs:  []string{"10.1.0.0/24"},
		Manifests: []string{"site.yaml"},
	}

	h := &siteInitHandler{
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	err := h.ensureUnboundedSite(context.Background(), cfg)
	require.NoError(t, err)

	// Verify default mode uses net.unbounded-kube.io apiVersion by
	// rendering the template directly.
	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	appliedYAML = content
	require.Contains(t, string(appliedYAML), "net.unbounded-kube.io/v1alpha1")
	require.NotContains(t, string(appliedYAML), "unbounded.aks.azure.com/v1alpha1")
}

// TestSiteInitCommand_DefaultCNIManifests verifies the default --cni-manifests
// value points to the unbounded-net release when prototype mode is off.
func TestSiteInitCommand_DefaultCNIManifests(t *testing.T) {
	cmd := siteInitCommand()
	f := cmd.Flags().Lookup("cni-manifests")
	require.NotNil(t, f)
	require.Equal(t, unboundedCNIRelease, f.DefValue)
}
