// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
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
		EnableMachina:   true,
		EnableMetalman:  true,
		EnableStorage:   true,
		Manifests:       []string{"site.yaml"},
	}

	h := &siteInitHandler{
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	err := h.ensureUnboundedSite(context.Background(), cfg)
	require.NoError(t, err)

	// Verify site init renders the promoted global Site API by
	// rendering the template directly.
	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	appliedYAML = content
	require.Contains(t, string(appliedYAML), "unbounded-cloud.io/v1alpha3")
	require.NotContains(t, string(appliedYAML), "net.unbounded-cloud.io/v1alpha1")
}

func TestSiteInitCommand_ComponentFlags(t *testing.T) {
	cmd := siteInitCommand()

	require.Nil(t, cmd.Flags().Lookup("cni-manifests"))
	require.Nil(t, cmd.Flags().Lookup("machina-manifests"))
	require.Nil(t, cmd.Flags().Lookup("enable-net"))

	flag := cmd.Flags().Lookup("enable-machina")
	require.NotNil(t, flag)
	require.Equal(t, "true", flag.DefValue)

	flag = cmd.Flags().Lookup("enable-metalman")
	require.NotNil(t, flag)
	require.Equal(t, "false", flag.DefValue)

	flag = cmd.Flags().Lookup("enable-storage")
	require.NotNil(t, flag)
	require.Equal(t, "false", flag.DefValue)

	flag = cmd.Flags().Lookup("skip-install")
	require.NotNil(t, flag)
	require.Equal(t, "false", flag.DefValue)

	flag = cmd.Flags().Lookup("install-timeout")
	require.NotNil(t, flag)
	require.Equal(t, defaultInstallTimeout.String(), flag.DefValue)
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
		EnableMachina:   true,
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
		EnableMachina:   true,
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

func TestEnsureUnboundedSite_ComponentConfig(t *testing.T) {
	cfg := unboundedSiteConfig{
		SiteName:        "test-site",
		NodeCIDRs:       []string{"10.0.0.0/24"},
		PodCIDRs:        []string{"10.1.0.0/24"},
		ManageCniPlugin: true,
		EnableMachina:   true,
		EnableMetalman:  true,
		EnableStorage:   true,
	}

	content, err := siteTemplates.ReadFile("assets/unbounded-net-site/site.yaml")
	require.NoError(t, err)

	tmpl, err := template.New("site.yaml").Parse(string(content))
	require.NoError(t, err)

	var buf strings.Builder
	require.NoError(t, tmpl.Execute(&buf, cfg))

	rendered := buf.String()
	assert.Contains(t, rendered, "apiVersion: unbounded-cloud.io/v1alpha3")
	assert.NotContains(t, rendered, "net:")
	assert.Contains(t, rendered, "machina:\n      enabled: true")
	assert.Contains(t, rendered, "metalman:\n      enabled: true")
	assert.Contains(t, rendered, "storage:\n      enabled: true")
}

func TestSiteInitComponentOwnership(t *testing.T) {
	h := &siteInitHandler{
		name:            "edge",
		clusterNodeCIDR: "10.0.0.0/24",
		clusterPodCIDR:  "10.1.0.0/24",
		nodeCIDR:        "10.2.0.0/24",
		podCIDR:         "10.3.0.0/24",
		manageCniPlugin: true,
		enableMachina:   true,
		enableMetalman:  true,
		enableStorage:   true,
	}

	cluster := h.clusterSiteConfig()
	assert.True(t, cluster.EnableMachina)
	assert.False(t, cluster.EnableStorage)
	assert.False(t, cluster.EnableMetalman)

	remote := h.remoteSiteConfig()
	assert.False(t, remote.EnableMachina)
	assert.True(t, remote.EnableStorage)
	assert.True(t, remote.EnableMetalman)
}

func TestSiteInitValidateClusterCIDRMessages(t *testing.T) {
	// A valid baseline handler; each case perturbs exactly one cluster CIDR
	// field to assert the error message names the correct field (guarding
	// against the previously swapped node/pod messages).
	base := func() *siteInitHandler {
		return &siteInitHandler{
			name:            "edge",
			clusterNodeCIDR: "10.0.0.0/24",
			clusterPodCIDR:  "10.1.0.0/24",
			nodeCIDR:        "10.2.0.0/24",
			podCIDR:         "10.3.0.0/24",
		}
	}

	cases := []struct {
		name    string
		mutate  func(*siteInitHandler)
		wantErr string
	}{
		{
			name:    "missing cluster node CIDR",
			mutate:  func(h *siteInitHandler) { h.clusterNodeCIDR = "" },
			wantErr: "cluster node CIDR is required",
		},
		{
			name:    "invalid cluster node CIDR",
			mutate:  func(h *siteInitHandler) { h.clusterNodeCIDR = "not-a-cidr" },
			wantErr: "cluster node CIDR is invalid",
		},
		{
			name:    "missing cluster pod CIDR",
			mutate:  func(h *siteInitHandler) { h.clusterPodCIDR = "" },
			wantErr: "cluster pod CIDR is required",
		},
		{
			name:    "invalid cluster pod CIDR",
			mutate:  func(h *siteInitHandler) { h.clusterPodCIDR = "not-a-cidr" },
			wantErr: "cluster pod CIDR is invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := base()
			tc.mutate(h)

			err := h.validate()
			require.Error(t, err)
			require.EqualError(t, err, tc.wantErr)
		})
	}
}
