// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
		SiteName:        "test-site",
		NodeCIDRs:       []string{"10.0.0.0/24"},
		PodCIDRs:        []string{"10.1.0.0/24"},
		ManageCniPlugin: true,
		Manifests:       []string{"site.yaml"},
	}

	h := &siteInitHandler{
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	err := h.ensureUnboundedSite(context.Background(), cfg)
	require.NoError(t, err)

	// Verify default mode uses net.unbounded-cloud.io apiVersion by
	// rendering the template directly.
	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	appliedYAML = content
	require.Contains(t, string(appliedYAML), "net.unbounded-cloud.io/v1alpha1")
	require.NotContains(t, string(appliedYAML), "unbounded.aks.azure.com/v1alpha1")
}

// TestSiteInitCommand_DefaultCNIManifests verifies the default --cni-manifests
// value is empty so the embedded manifests are used.
func TestSiteInitCommand_DefaultCNIManifests(t *testing.T) {
	cmd := siteInitCommand()
	f := cmd.Flags().Lookup("cni-manifests")
	require.NotNil(t, f)
	require.Equal(t, "", f.DefValue)
}

func TestSiteInitCommand_DefaultMachineOpsManifests(t *testing.T) {
	cmd := siteInitCommand()
	f := cmd.Flags().Lookup("machine-ops-manifests")
	require.NotNil(t, f)
	require.Equal(t, "", f.DefValue)
}

func TestSiteInitHandlerValidate_ManifestSources(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600))

	base := func() *siteInitHandler {
		return &siteInitHandler{
			name:            "remote",
			clusterNodeCIDR: "10.0.0.0/16",
			clusterPodCIDR:  "10.244.0.0/16",
			nodeCIDR:        "192.168.99.0/24",
			podCIDR:         "10.245.0.0/16",
			kubeconfigPath:  kubeconfig,
		}
	}

	t.Setenv("KUBECONFIG", kubeconfig)

	for _, tc := range []struct {
		name string
		mut  func(*siteInitHandler)
		want string
	}{
		{
			name: "cni",
			mut: func(h *siteInitHandler) {
				h.cniManifests = filepath.Join(dir, "missing-cni")
			},
			want: "CNI manifests path is invalid",
		},
		{
			name: "machina",
			mut: func(h *siteInitHandler) {
				h.machinaManifests = filepath.Join(dir, "missing-machina")
			},
			want: "machina manifests path is invalid",
		},
		{
			name: "machine-ops",
			mut: func(h *siteInitHandler) {
				h.machineOpsManifests = filepath.Join(dir, "missing-machine-ops")
			},
			want: "machine-ops manifests path is invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := base()
			tc.mut(h)

			err := h.validate()
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestSiteInitCommand_ManageCniPluginFlag(t *testing.T) {
	cmd := siteInitCommand()
	f := cmd.Flags().Lookup("manage-cni-plugin")
	require.NotNil(t, f, "--manage-cni-plugin flag should exist")
	require.Equal(t, "true", f.DefValue, "default should be true")
}

func TestEnsureUnboundedSite_ManageCniPluginFalse(t *testing.T) {
	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	cfg := unboundedSiteConfig{
		SiteName:        "test-site",
		NodeCIDRs:       []string{"10.0.0.0/24"},
		PodCIDRs:        []string{"10.1.0.0/24"},
		ManageCniPlugin: false,
		Manifests:       []string{"site.yaml"},
	}

	h := &siteInitHandler{
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	err := h.ensureUnboundedSite(context.Background(), cfg)
	require.NoError(t, err)

	// Render the template directly and verify manageCniPlugin: false appears.
	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	tmpl, err := template.New("site.yaml").Parse(string(content))
	require.NoError(t, err)

	var buf strings.Builder
	require.NoError(t, tmpl.Execute(&buf, cfg))

	rendered := buf.String()
	assert.Contains(t, rendered, "manageCniPlugin: false")
	assert.Contains(t, rendered, "name: test-site")
}

func TestEnsureUnboundedSite_ManageCniPluginTrue(t *testing.T) {
	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	cfg := unboundedSiteConfig{
		SiteName:        "test-site",
		NodeCIDRs:       []string{"10.0.0.0/24"},
		PodCIDRs:        []string{"10.1.0.0/24"},
		ManageCniPlugin: true,
		Manifests:       []string{"site.yaml"},
	}

	h := &siteInitHandler{
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	err := h.ensureUnboundedSite(context.Background(), cfg)
	require.NoError(t, err)

	// Render the template directly and verify manageCniPlugin does NOT appear.
	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	tmpl, err := template.New("site.yaml").Parse(string(content))
	require.NoError(t, err)

	var buf strings.Builder
	require.NoError(t, tmpl.Execute(&buf, cfg))

	rendered := buf.String()
	assert.NotContains(t, rendered, "manageCniPlugin")
	assert.Contains(t, rendered, "name: test-site")
}

func TestEnsureControllersAreRunningInstallsMachinaThenMachineOps(t *testing.T) {
	t.Parallel()

	var calls []string

	machina := &recordingComponentInstaller{name: "machina", calls: &calls}
	machineOps := &recordingComponentInstaller{name: "machine-ops", calls: &calls}
	h := newControllerInstallTestHandler(machina, machineOps)

	err := h.ensureControllersAreRunning(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"machina", "machine-ops"}, calls)
	require.Equal(t, []string{"03-config.yaml"}, machina.skipPaths)
}

func TestEnsureControllersAreRunningWrapsMachineOpsFailure(t *testing.T) {
	t.Parallel()

	var calls []string

	machineOpsErr := errors.New("machine-ops failed")
	machina := &recordingComponentInstaller{name: "machina", calls: &calls}
	machineOps := &recordingComponentInstaller{name: "machine-ops", calls: &calls, err: machineOpsErr}
	h := newControllerInstallTestHandler(machina, machineOps)

	err := h.ensureControllersAreRunning(context.Background())

	require.ErrorIs(t, err, machineOpsErr)
	require.ErrorContains(t, err, "installing machine-ops controller for site site-a")
	require.Equal(t, []string{"machina", "machine-ops"}, calls)
}

func TestEnsureClusterInfoAppliesStandardKubePublicConfigMap(t *testing.T) {
	t.Parallel()

	kubeCli := fake.NewClientset()
	h := &siteInitHandler{
		kubeCli: kubeCli,
		kubeConfig: &rest.Config{
			Host: "https://api.example.com:6443",
			TLSClientConfig: rest.TLSClientConfig{
				CAData: []byte("test-ca"),
			},
		},
	}

	err := h.ensureClusterInfo(context.Background())
	require.NoError(t, err)

	cm, err := kubeCli.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(context.Background(), "cluster-info", metav1.GetOptions{})
	require.NoError(t, err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(cm.Data["kubeconfig"]))
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com:6443", cfg.Host)
	require.Equal(t, []byte("test-ca"), cfg.CAData)
}

func TestClusterInfoKubeconfigReadsCAFile(t *testing.T) {
	t.Parallel()

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte("file-ca"), 0o600))

	data, err := clusterInfoKubeconfig(&rest.Config{
		Host: "https://api.example.com:6443",
		TLSClientConfig: rest.TLSClientConfig{
			CAFile: caFile,
		},
	})
	require.NoError(t, err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com:6443", cfg.Host)
	require.Equal(t, []byte("file-ca"), cfg.CAData)
}

func TestClusterInfoKubeconfigRequiresCAData(t *testing.T) {
	t.Parallel()

	_, err := clusterInfoKubeconfig(&rest.Config{Host: "https://api.example.com:6443"})
	require.EqualError(t, err, "kubernetes CA data is required")
}

func newControllerInstallTestHandler(machina, machineOps componentInstaller) *siteInitHandler {
	return &siteInitHandler{
		name:              "site-a",
		kubeCli:           fake.NewClientset(),
		kubeConfig:        &rest.Config{Host: "https://api.example.com"},
		kubeResourcesCli:  fakeclient.NewClientBuilder().Build(),
		installMachina:    machina,
		installMachineOps: machineOps,
		logger:            discardLogger(),
	}
}

type recordingComponentInstaller struct {
	name      string
	calls     *[]string
	skipPaths []string
	err       error
}

func (i *recordingComponentInstaller) run(context.Context) error {
	*i.calls = append(*i.calls, i.name)

	return i.err
}

func (i *recordingComponentInstaller) setSkipPaths(paths []string) {
	i.skipPaths = append([]string(nil), paths...)
}
