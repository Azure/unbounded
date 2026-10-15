//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operatore2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/machina"
)

// Exercise production manifests and SSA without building component images. A
// newly constructed reconciler models loss of all process-local state.
func assertFreshConfigAndRestart(t *testing.T, c client.Client) {
	t.Helper()
	ctx := t.Context()
	site := overrideTestSite()
	site.Name = "fresh-config"
	site.Spec.Components.Machina = &machinav1.MachinaComponentSpec{
		SiteComponentSpec: machinav1.SiteComponentSpec{Enabled: ptr.To(true)},
	}
	require.NoError(t, c.Create(ctx, site))

	newReconciler := func() *operator.SiteReconciler {
		return &operator.SiteReconciler{
			Client: c, APIReader: c, Scheme: c.Scheme(), Namespace: overridesNamespace,
			Config:   operator.Config{ImageRegistry: "example.test", ImageTag: "e2e", APIServerEndpoint: "https://api.example.test:6443"},
			Registry: &component.Registry{Cluster: []component.ClusterComponent{machina.New()}},
		}
	}
	reconcile := func(r *operator.SiteReconciler) {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: site.Name}})
		require.NoError(t, err)
	}
	r := newReconciler()
	reconcile(r)

	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: overridesNamespace, Name: name} }
	config := &corev1.ConfigMap{}
	deployment := &appsv1.Deployment{}

	require.NoError(t, c.Get(ctx, key("machina-config"), config))
	require.NoError(t, c.Get(ctx, key("machina-controller"), deployment))
	uid := deployment.UID
	before := deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation]
	config.Data["e2e-note"] = "preserve user data"
	config.BinaryData = map[string][]byte{"e2e.bin": {0, 1, 255}}
	require.NoError(t, c.Update(ctx, config))
	want := config.DeepCopy()

	reconcile(r)
	require.NoError(t, c.Get(ctx, key("machina-controller"), deployment))
	require.NotEqual(t, before, deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation])
	require.Equal(t, component.ConfigMapPayloadHash(want), deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation])
	before = deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation]
	config.BinaryData["e2e.bin"] = []byte{2, 3}
	require.NoError(t, c.Update(ctx, config))
	want = config.DeepCopy()

	reconcile(newReconciler())
	require.NoError(t, c.Get(ctx, key("machina-controller"), deployment))
	require.Equal(t, uid, deployment.UID)
	require.NotEqual(t, before, deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation])
	require.Equal(t, component.ConfigMapPayloadHash(want), deployment.Spec.Template.Annotations[machina.ConfigHashAnnotation])
	require.NoError(t, c.Get(ctx, key("machina-config"), config))
	require.Equal(t, want.Data, config.Data)
	require.Equal(t, want.BinaryData, config.BinaryData)
}

func assertCRDRepair(t *testing.T, kubeconfig string, c client.Client) {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	assertCRDRepairWithConfig(t, cfg, c)
}

func assertCRDRepairWithConfig(t *testing.T, cfg *rest.Config, c client.Client) {
	t.Helper()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: c.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	require.NoError(t, (&operator.CRDReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	defer func() { cancel(); require.NoError(t, <-done) }()

	require.True(t, mgr.GetCache().WaitForCacheSync(ctx))

	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "machineoperationcredentials.unbounded-cloud.io"}}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(crd), crd))
	uid := crd.UID
	require.NoError(t, c.Delete(ctx, crd))
	require.Eventually(t, func() bool {
		if c.Get(ctx, client.ObjectKeyFromObject(crd), crd) != nil || crd.UID == uid {
			return false
		}

		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				return true
			}
		}

		return false
	}, 30*time.Second, 100*time.Millisecond)

	uid = crd.UID

	require.NoError(t, operator.BootstrapCRDs(ctx, c))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(crd), crd))
	require.Equal(t, uid, crd.UID, "startup bootstrap must be idempotent after repair")
}

// Also exercise the fresh-install assertions without kind or live workloads.
func TestEnvtestFreshOperator(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for local API-server fresh-install assertions")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	require.NoError(t, machinav1.AddToScheme(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, operator.BootstrapCRDs(t.Context(), c))
	assertFreshConfigAndRestart(t, c)
	assertCRDRepairWithConfig(t, cfg, c)
}
