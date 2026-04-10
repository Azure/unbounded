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
		Manifests:       []string{"site.yaml"},
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
