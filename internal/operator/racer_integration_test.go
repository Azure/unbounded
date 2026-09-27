// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	racercore "github.com/Azure/unbounded/internal/racer"
)

// Exercise startup CRD bootstrap, informer-driven activation without a Site,
// the real executor/SSA, and the controller's initialization and workload paths.
// Envtest has no kubelet: execute the observed Job's initialize action directly.
func TestEnvtestRacerProvisioning(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server provisioning")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, machinav1.AddToScheme, racerv1.AddToScheme} {
		require.NoError(t, add(scheme))
	}

	c, err := client.New(rc, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, BootstrapCRDs(t.Context(), c))

	const namespace = "racer-provisioning"
	require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

	mgr, err := ctrl.NewManager(rc, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	require.NoError(t, err)

	r := &SiteReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: scheme, Namespace: namespace, Config: Config{ImageRegistry: "example.test", ImageTag: "v1"}}
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx))

	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: namespace, Name: name} }
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key("racer-controller"), &appsv1.Deployment{})))
	require.NoError(t, c.Create(ctx, &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry"}}))

	job := &batchv1.Job{}

	require.Eventually(t, func() bool { return c.Get(ctx, key("racer-initialize"), job) == nil }, 45*time.Second, 100*time.Millisecond)
	require.Equal(t, []string{"initialize"}, job.Spec.Template.Spec.Containers[0].Args)
	require.Equal(t, "example.test/racer-controller:v1", job.Spec.Template.Spec.Containers[0].Image)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key("racer-controller"), &appsv1.Deployment{})))

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, key("racer-config"), cm))

	for name, value := range cm.Data {
		t.Setenv(name, value)
	}

	t.Setenv("POD_NAMESPACE", namespace)

	cfg, err := racercore.LoadConfig()
	require.NoError(t, err)

	app := racercore.Assemble(cfg, c, c)
	require.NoError(t, app.Topology.InitializeVersion(ctx))

	deployment := &appsv1.Deployment{}

	require.Eventually(t, func() bool { return c.Get(ctx, key("racer-controller"), deployment) == nil }, 20*time.Second, 100*time.Millisecond)
	require.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
	require.Nil(t, deployment.Spec.Strategy.RollingUpdate)

	_, err = app.Keyring.Reconcile(ctx, ctrl.Request{})
	require.NoError(t, err)
	_, err = app.Workload.Reconcile(ctx, ctrl.Request{})
	require.NoError(t, err)

	ds := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(ctx, key("racer-dataplane"), ds))
	require.Equal(t, "example.test/racer-dataplane:v1", ds.Spec.Template.Spec.Containers[0].Image)

	for _, name := range []string{"racer-controller-tls", "racer-issuer", "racer-keyring"} {
		require.NoError(t, c.Get(ctx, key(name), &corev1.Secret{}))
	}
	// Lost counters must not make either provisioner or normal startup reset.
	require.NoError(t, c.Get(ctx, key("racer-version"), cm))
	require.NoError(t, c.Delete(ctx, cm))
	_, err = r.Reconcile(ctx, ctrl.Request{})
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key("racer-version"), &corev1.ConfigMap{})))
	require.Error(t, app.Topology.InitializeVersion(ctx))
}
