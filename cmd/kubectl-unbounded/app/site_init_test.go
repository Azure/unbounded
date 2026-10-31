// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/kube"
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
		SiteName:             "test-site",
		NodeCIDRs:            []string{"10.0.0.0/24"},
		PodCIDRs:             []string{"10.1.0.0/24"},
		ManageCniPlugin:      true,
		EnableMachina:        true,
		EnableMetalman:       true,
		EnableTokenRefresher: true,
		Manifests:            []string{"site.yaml"},
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

	// site init no longer bootstraps the operator; the operator must be
	// installed first via `kubectl unbounded install`. The install lifecycle
	// flags were removed.
	require.Nil(t, cmd.Flags().Lookup("skip-install"))
	require.Nil(t, cmd.Flags().Lookup("install-timeout"))

	// site init creates one Site per run, so the flags for the second,
	// implicit cluster Site were removed.
	require.Nil(t, cmd.Flags().Lookup("cluster-node-cidr"))
	require.Nil(t, cmd.Flags().Lookup("cluster-pod-cidr"))
}

func TestSiteInitCommand_CIDRFlags(t *testing.T) {
	cmd := siteInitCommand()

	for _, name := range []string{"node-cidr", "pod-cidr"} {
		flag := cmd.Flags().Lookup(name)
		require.NotNil(t, flag, "--%s flag should exist", name)
		require.Equal(t, "stringSlice", flag.Value.Type(), "--%s should take a comma-separated list", name)
		require.Equal(t, []string{"true"}, flag.Annotations[cobra.BashCompOneRequiredFlag], "--%s should be required", name)
	}

	require.NoError(t, cmd.ParseFlags([]string{
		"--node-cidr", "10.0.0.0/24,10.1.0.0/24",
		"--pod-cidr", "10.244.0.0/16",
	}))

	nodeCIDRs, err := cmd.Flags().GetStringSlice("node-cidr")
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.0/24", "10.1.0.0/24"}, nodeCIDRs)

	podCIDRs, err := cmd.Flags().GetStringSlice("pod-cidr")
	require.NoError(t, err)
	require.Equal(t, []string{"10.244.0.0/16"}, podCIDRs)
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
		SiteName:             "test-site",
		NodeCIDRs:            []string{"10.0.0.0/24"},
		PodCIDRs:             []string{"10.1.0.0/24"},
		ManageCniPlugin:      true,
		EnableMachina:        true,
		EnableMetalman:       true,
		EnableTokenRefresher: true,
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
	assert.Contains(t, rendered, "tokenRefresher:\n      enabled: true")
}

func TestSiteInitSiteConfig(t *testing.T) {
	cases := []struct {
		name               string
		manageCniPlugin    bool
		enableMachina      bool
		enableMetalman     bool
		wantTokenRefresher bool
	}{
		{
			name:               "flex-site",
			manageCniPlugin:    true,
			enableMachina:      true,
			enableMetalman:     true,
			wantTokenRefresher: true,
		},
		{
			// The token refresher ignores the reserved cluster Site.
			name:               clusterSiteName,
			wantTokenRefresher: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &siteInitHandler{
				name:            tc.name,
				nodeCIDRs:       []string{"10.0.0.0/24", "10.2.0.0/24"},
				podCIDRs:        []string{"10.1.0.0/24", "10.3.0.0/24"},
				manageCniPlugin: tc.manageCniPlugin,
				enableMachina:   tc.enableMachina,
				enableMetalman:  tc.enableMetalman,
			}

			cfg := h.siteConfig()
			assert.Equal(t, tc.name, cfg.SiteName)
			assert.Equal(t, []string{"10.0.0.0/24", "10.2.0.0/24"}, cfg.NodeCIDRs)
			assert.Equal(t, []string{"10.1.0.0/24", "10.3.0.0/24"}, cfg.PodCIDRs)
			assert.Equal(t, tc.manageCniPlugin, cfg.ManageCniPlugin)
			assert.Equal(t, tc.enableMachina, cfg.EnableMachina)
			assert.Equal(t, tc.enableMetalman, cfg.EnableMetalman)
			assert.Equal(t, tc.wantTokenRefresher, cfg.EnableTokenRefresher)
			assert.Equal(t, []string{"site.yaml"}, cfg.Manifests)
		})
	}
}

func TestSiteInitValidate(t *testing.T) {
	dir := t.TempDir()

	kubeconfig := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600))

	missingKubeconfig := filepath.Join(dir, "missing")

	// A valid baseline handler; each case changes one input.
	base := func() *siteInitHandler {
		return &siteInitHandler{
			name:           "edge",
			nodeCIDRs:      []string{"10.0.0.0/24"},
			podCIDRs:       []string{"10.1.0.0/24"},
			kubeconfigPath: kubeconfig,
		}
	}

	cases := []struct {
		name     string
		mutate   func(*siteInitHandler)
		wantNode []string
		wantPod  []string
		wantErr  string
	}{
		{
			name:     "single CIDRs",
			mutate:   func(*siteInitHandler) {},
			wantNode: []string{"10.0.0.0/24"},
			wantPod:  []string{"10.1.0.0/24"},
		},
		{
			name: "lists keep their order and lose surrounding spaces",
			mutate: func(h *siteInitHandler) {
				h.nodeCIDRs = []string{"10.0.0.0/24", " 10.2.0.0/24 "}
				h.podCIDRs = []string{" 10.1.0.0/24", "10.3.0.0/24 "}
			},
			wantNode: []string{"10.0.0.0/24", "10.2.0.0/24"},
			wantPod:  []string{"10.1.0.0/24", "10.3.0.0/24"},
		},
		{
			// Node CIDRs of one Site may overlap; only a repeated network is
			// rejected.
			name:     "overlapping node CIDRs",
			mutate:   func(h *siteInitHandler) { h.nodeCIDRs = []string{"10.0.0.0/16", "10.0.1.0/24"} },
			wantNode: []string{"10.0.0.0/16", "10.0.1.0/24"},
			wantPod:  []string{"10.1.0.0/24"},
		},
		{
			name:    "missing name",
			mutate:  func(h *siteInitHandler) { h.name = "" },
			wantErr: "site name is required",
		},
		{
			name:    "no node CIDRs",
			mutate:  func(h *siteInitHandler) { h.nodeCIDRs = nil },
			wantErr: "--node-cidr requires at least one CIDR",
		},
		{
			// pflag parses --node-cidr "" into an empty list.
			name:    "empty node CIDR list",
			mutate:  func(h *siteInitHandler) { h.nodeCIDRs = []string{} },
			wantErr: "--node-cidr requires at least one CIDR",
		},
		{
			name:    "empty node CIDR entry",
			mutate:  func(h *siteInitHandler) { h.nodeCIDRs = []string{"10.0.0.0/24", " "} },
			wantErr: "--node-cidr contains an empty entry",
		},
		{
			name:    "invalid node CIDR among valid ones",
			mutate:  func(h *siteInitHandler) { h.nodeCIDRs = []string{"10.0.0.0/24", "10.2.0.0/33"} },
			wantErr: `--node-cidr value "10.2.0.0/33" is not a valid IPv4 CIDR`,
		},
		{
			name:    "repeated node network",
			mutate:  func(h *siteInitHandler) { h.nodeCIDRs = []string{"10.0.0.0/24", "10.0.0.5/24"} },
			wantErr: `--node-cidr lists network 10.0.0.0/24 more than once ("10.0.0.0/24" and "10.0.0.5/24")`,
		},
		{
			name:    "no pod CIDRs",
			mutate:  func(h *siteInitHandler) { h.podCIDRs = nil },
			wantErr: "--pod-cidr requires at least one CIDR",
		},
		{
			name:    "empty pod CIDR entry",
			mutate:  func(h *siteInitHandler) { h.podCIDRs = []string{"", "10.1.0.0/24"} },
			wantErr: "--pod-cidr contains an empty entry",
		},
		{
			name:    "IPv6 pod CIDR",
			mutate:  func(h *siteInitHandler) { h.podCIDRs = []string{"fd00::/48"} },
			wantErr: `--pod-cidr value "fd00::/48" is not a valid IPv4 CIDR`,
		},
		{
			name:    "repeated pod network",
			mutate:  func(h *siteInitHandler) { h.podCIDRs = []string{"10.1.0.0/24", "10.1.0.0/24"} },
			wantErr: `--pod-cidr lists network 10.1.0.0/24 more than once ("10.1.0.0/24" and "10.1.0.0/24")`,
		},
		{
			name:    "overlapping pod CIDRs",
			mutate:  func(h *siteInitHandler) { h.podCIDRs = []string{"10.244.0.0/16", "10.244.1.0/24"} },
			wantErr: `--pod-cidr values "10.244.0.0/16" and "10.244.1.0/24" overlap`,
		},
		{
			name:    "unreadable kubeconfig",
			mutate:  func(h *siteInitHandler) { h.kubeconfigPath = missingKubeconfig },
			wantErr: fmt.Sprintf("kubeconfig %q not readable", missingKubeconfig),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := base()
			tc.mutate(h)

			err := h.validate()
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantNode, h.nodeCIDRs)
			assert.Equal(t, tc.wantPod, h.podCIDRs)
		})
	}
}

func TestRenderSiteManifests(t *testing.T) {
	h := &siteInitHandler{
		name:            "flex-site",
		nodeCIDRs:       []string{"10.100.0.0/24", "10.110.0.0/24"},
		podCIDRs:        []string{"10.101.0.0/24", "10.111.0.0/24"},
		manageCniPlugin: true,
		enableMachina:   true,
	}

	manifests, err := renderSiteManifests(h.siteConfig())
	require.NoError(t, err)

	var objects []*unstructured.Unstructured

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)

	for {
		obj := &unstructured.Unstructured{}

		err := decoder.Decode(obj)
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		if obj.Object != nil {
			objects = append(objects, obj)
		}
	}

	// Exactly one Site: site init no longer renders a GatewayPool or a
	// SiteGatewayPoolAssignment.
	require.Len(t, objects, 1)

	site := objects[0]
	assert.Equal(t, "unbounded-cloud.io/v1alpha3", site.GetAPIVersion())
	assert.Equal(t, "Site", site.GetKind())
	assert.Equal(t, "flex-site", site.GetName())

	nodeCIDRs, found, err := unstructured.NestedStringSlice(site.Object, "spec", "nodeCidrs")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"10.100.0.0/24", "10.110.0.0/24"}, nodeCIDRs)

	assignments, found, err := unstructured.NestedSlice(site.Object, "spec", "podCidrAssignments")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, assignments, 1)

	assignment, ok := assignments[0].(map[string]any)
	require.True(t, ok)

	podCIDRs, found, err := unstructured.NestedStringSlice(assignment, "cidrBlocks")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"10.101.0.0/24", "10.111.0.0/24"}, podCIDRs)
}

func TestSiteInitRun(t *testing.T) {
	cases := []struct {
		name      string
		wantToken bool
	}{
		{name: "flex-site", wantToken: true},
		{
			// Nothing refreshes a token for the reserved cluster Site, so site
			// init does not create one.
			name:      clusterSiteName,
			wantToken: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var applied []string

			kubeResourcesCli := fakeclient.NewClientBuilder().
				WithInterceptorFuncs(interceptor.Funcs{
					Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
						meta, ok := obj.(interface {
							GetKind() string
							GetName() string
						})
						require.True(t, ok, "applied configuration should expose its kind and name")

						applied = append(applied, meta.GetKind()+"/"+meta.GetName())

						return nil
					},
				}).
				Build()

			kubeCli := fakeClusterServing(servedSiteAPIs())

			h := &siteInitHandler{
				name:             tc.name,
				nodeCIDRs:        []string{"10.100.0.0/24"},
				podCIDRs:         []string{"10.101.0.0/24"},
				manageCniPlugin:  true,
				enableMachina:    true,
				kubeCli:          kubeCli,
				kubeResourcesCli: kubeResourcesCli,
				logger:           discardLogger(),
			}

			require.NoError(t, h.run(context.Background()))
			assert.Equal(t, []string{"Site/" + tc.name}, applied)

			_, err := kube.GetBootstrapTokenForSite(context.Background(), kubeCli, tc.name)
			if tc.wantToken {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, kube.ErrBootstrapTokenNotFound)
			}
		})
	}
}

// TestSiteInitRunReusesValidBootstrapToken pins the contract the help text
// states: a Site that already has a valid bootstrap token keeps it, and an
// expired one is replaced.
func TestSiteInitRunReusesValidBootstrapToken(t *testing.T) {
	const existingID = "abcdef"

	cases := []struct {
		name        string
		expiresAt   time.Time
		wantSecrets int
		wantReused  bool
	}{
		{
			name:        "valid token is reused",
			expiresAt:   time.Now().Add(time.Hour),
			wantSecrets: 1,
			wantReused:  true,
		},
		{
			name:        "expired token is replaced",
			expiresAt:   time.Now().Add(-time.Hour),
			wantSecrets: 2,
			wantReused:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      kube.BootstrapTokenSecretName(existingID),
					Namespace: metav1.NamespaceSystem,
					Labels:    map[string]string{"unbounded-cloud.io/site": "flex-site"},
				},
				Type: corev1.SecretTypeBootstrapToken,
				Data: map[string][]byte{
					"token-id":     []byte(existingID),
					"token-secret": []byte("0123456789abcdef"),
					"expiration":   []byte(tc.expiresAt.UTC().Format(time.RFC3339)),
				},
			}

			kubeCli := fakeClusterServing(servedSiteAPIs(), existing)

			kubeResourcesCli := fakeclient.NewClientBuilder().
				WithInterceptorFuncs(interceptor.Funcs{
					Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
						return nil
					},
				}).
				Build()

			h := &siteInitHandler{
				name:             "flex-site",
				nodeCIDRs:        []string{"10.100.0.0/24"},
				podCIDRs:         []string{"10.101.0.0/24"},
				kubeCli:          kubeCli,
				kubeResourcesCli: kubeResourcesCli,
				logger:           discardLogger(),
			}

			require.NoError(t, h.run(ctx))

			secrets, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			assert.Len(t, secrets.Items, tc.wantSecrets)

			token, err := kube.GetBootstrapTokenForSite(ctx, kubeCli, "flex-site")
			require.NoError(t, err)
			assert.Equal(t, tc.wantReused, token.ID == existingID)
		})
	}
}

// ---------------------------------------------------------------------------
// checkOperatorPrerequisites tests
// ---------------------------------------------------------------------------

// bufferLogger returns a slog.Logger that writes warn-level records to buf so
// tests can assert on the operator-readiness warnings.
func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}

	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf
}

// servedSiteAPIs returns the discovery resource lists for a cluster that serves
// every API type site init applies. Individual tests drop entries to simulate an
// operator that is not (yet) installed.
func servedSiteAPIs() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "unbounded-cloud.io/v1alpha3",
			APIResources: []metav1.APIResource{
				{Name: "sites", Kind: "Site"},
			},
		},
	}
}

// fakeClusterServing builds a fake clientset whose discovery serves the given
// resource lists and whose objects seed the typed tracker.
func fakeClusterServing(resources []*metav1.APIResourceList, objects ...runtime.Object) *k8sfake.Clientset {
	cs := k8sfake.NewClientset(objects...)
	cs.Resources = resources

	return cs
}

func TestRequiredSiteAPIs(t *testing.T) {
	gvks, err := requiredSiteAPIs()
	require.NoError(t, err)

	// site init applies only the Site; gateway resources are created
	// separately with `kubectl unbounded net create`.
	require.ElementsMatch(t, []schema.GroupVersionKind{
		{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Site"},
	}, gvks)
}

func TestCheckOperatorPrerequisites_APITypesNotServed(t *testing.T) {
	cli := fakeClusterServing(nil)

	logger, _ := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	err := h.checkOperatorPrerequisites(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unbounded-operator is not installed")
	require.Contains(t, err.Error(), "kubectl unbounded install")
	require.Contains(t, err.Error(), "Site.unbounded-cloud.io/v1alpha3")
}

// TestCheckOperatorPrerequisites_SiteAPIOnly proves the preflight needs only the
// Site API: site init no longer applies the net-group gateway types, so a
// cluster that does not serve them passes.
func TestCheckOperatorPrerequisites_SiteAPIOnly(t *testing.T) {
	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           1,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})
	cli := fakeClusterServing([]*metav1.APIResourceList{{
		GroupVersion: "unbounded-cloud.io/v1alpha3",
		APIResources: []metav1.APIResource{{Name: "sites", Kind: "Site"}},
	}}, deploy)

	logger, logs := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	require.NoError(t, h.checkOperatorPrerequisites(context.Background()))
	require.Empty(t, logs.String())
}

func TestCheckOperatorPrerequisites_OperatorAbsentWarns(t *testing.T) {
	// All required types are served but the operator Deployment is absent: site
	// init must proceed (no error) and only warn.
	cli := fakeClusterServing(servedSiteAPIs())

	logger, logs := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	require.NoError(t, h.checkOperatorPrerequisites(context.Background()))
	require.Contains(t, logs.String(), "unbounded-operator Deployment not found")
}

func TestCheckOperatorPrerequisites_OperatorNotRolledOutWarns(t *testing.T) {
	// Old pod Available while the new pod surges in: rollout is not complete.
	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           2,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})
	cli := fakeClusterServing(servedSiteAPIs(), deploy)

	logger, logs := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	require.NoError(t, h.checkOperatorPrerequisites(context.Background()))
	require.Contains(t, logs.String(), "not fully rolled out")
}

func TestCheckOperatorPrerequisites_Ready(t *testing.T) {
	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           1,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})
	cli := fakeClusterServing(servedSiteAPIs(), deploy)

	logger, logs := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	require.NoError(t, h.checkOperatorPrerequisites(context.Background()))
	require.Empty(t, logs.String(), "a ready operator must not produce warnings")
}

// TestCheckOperatorPrerequisites_DeploymentForbiddenDoesNotFail proves that a
// caller who may create Sites but cannot read Deployments is not blocked: the
// served-API check passes and the forbidden Deployment read is downgraded to a
// debug log (no warning, no error).
func TestCheckOperatorPrerequisites_DeploymentForbiddenDoesNotFail(t *testing.T) {
	cli := fakeClusterServing(servedSiteAPIs())
	cli.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"},
			"unbounded-operator",
			errors.New("not allowed"),
		)
	})

	logger, logs := bufferLogger()
	h := &siteInitHandler{kubeCli: cli, logger: logger}

	require.NoError(t, h.checkOperatorPrerequisites(context.Background()))
	require.Empty(t, logs.String(), "a forbidden Deployment read must not warn or fail")
}
