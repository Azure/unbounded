// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/override"
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
	// Adopt the old controller's exact immutable selector and keep its UID.
	legacy, err := racercore.DesiredDaemonSet(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, legacy, client.FieldOwner("racer-controller")))

	app := racercore.Assemble(cfg, c, c)
	require.NoError(t, app.Topology.InitializeVersion(ctx))

	deployment := &appsv1.Deployment{}

	require.Eventually(t, func() bool { return c.Get(ctx, key("racer-controller"), deployment) == nil }, 20*time.Second, 100*time.Millisecond)
	require.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
	require.Nil(t, deployment.Spec.Strategy.RollingUpdate)

	_, err = app.Keyring.Reconcile(ctx, ctrl.Request{})
	require.NoError(t, err)

	ds := &appsv1.DaemonSet{}

	require.Eventually(t, func() bool {
		return c.Get(ctx, key("racer-dataplane"), ds) == nil && len(ds.Spec.Template.Spec.Containers[0].EnvFrom) == 1
	}, 20*time.Second, 100*time.Millisecond)
	require.Equal(t, legacy.UID, ds.UID)
	require.Equal(t, "example.test/racer-dataplane:v1", ds.Spec.Template.Spec.Containers[0].Image)
	t.Run("config-rollout-and-SSA-overrides", func(t *testing.T) {
		integrationRacerOverrides(t, c, key, ds)
	})
	t.Run("runtime-write-scope", func(t *testing.T) {
		integrationRacerWriteScope(t, rc, c, namespace)
	})

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

func integrationRacerOverrides(t *testing.T, c client.Client, key func(string) client.ObjectKey, ds *appsv1.DaemonSet) {
	t.Helper()
	ctx := t.Context()
	uid := ds.UID
	selector := ds.Spec.Selector.DeepCopy()
	config := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, key("racer-dataplane-config"), config))

	oldHash := ds.Spec.Template.Annotations["unbounded-cloud.io/racer-config-hash"]
	config.Data["RACER_MAX_THREADS"] = "2"
	require.NoError(t, c.Update(ctx, config))
	require.Eventually(t, func() bool {
		return c.Get(ctx, key(ds.Name), ds) == nil && ds.Spec.Template.Annotations["unbounded-cloud.io/racer-config-hash"] != oldHash
	}, 20*time.Second, 100*time.Millisecond)

	document := `apiVersion: ` + override.APIVersion + `
overrides:
  - component: racer
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              hardware.example/rdma: "true"
            tolerations:
              - key: hardware.example/rdma
                operator: Exists
            containers:
              - name: dataplane
                resources:
                  requests:
                    cpu: "2"
                    memory: 2Gi
                    rdma.example/hca: "1"
                  limits:
                    memory: 4Gi
                    rdma.example/hca: "1"
                env:
                  - name: RACER_ENABLE_RDMA
                    value: "true"
                volumeMounts:
                  - name: fabric
                    mountPath: /etc/racer/fabric
                    readOnly: true
            volumes:
              - name: fabric
                configMap:
                  name: admin-fabric
`
	overrides := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: override.ConfigMapName, Namespace: ds.Namespace}, Data: map[string]string{"racer.yaml": document}}
	require.NoError(t, c.Create(ctx, overrides))
	require.Eventually(t, func() bool {
		return c.Get(ctx, key(ds.Name), ds) == nil && ds.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("2")) == 0
	}, 20*time.Second, 100*time.Millisecond)

	pod := ds.Spec.Template.Spec
	require.Equal(t, "true", pod.NodeSelector["hardware.example/rdma"])
	require.Len(t, pod.Tolerations, 1)
	require.True(t, slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "fabric" }))
	require.True(t, slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(v corev1.VolumeMount) bool { return v.Name == "fabric" }))
	require.True(t, slices.ContainsFunc(pod.Containers[0].Env, func(v corev1.EnvVar) bool { return v.Name == "RACER_ENABLE_RDMA" && v.Value == "true" }))
	require.Equal(t, resource.MustParse("1"), pod.Containers[0].Resources.Limits["rdma.example/hca"])
	require.Equal(t, uid, ds.UID)
	require.Equal(t, selector, ds.Spec.Selector)

	// Invalid customization must withhold the workload instead of reverting it.
	require.NoError(t, c.Get(ctx, key(override.ConfigMapName), overrides))
	overrides.Data["racer.yaml"] = strings.Replace(document, "cpu: \"2\"", "cpu: nonsense", 1)
	require.NoError(t, c.Update(ctx, overrides))
	// Reconcile directly with live reads for a deterministic invalid-input pass.
	r := &SiteReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Namespace: ds.Namespace, Config: Config{ImageRegistry: "example.test", ImageTag: "v1"}}
	_, err := r.Reconcile(ctx, singletonRequest())
	require.Error(t, err)
	require.NoError(t, c.Get(ctx, key(ds.Name), ds))
	require.Equal(t, pod, ds.Spec.Template.Spec)

	// Actual SSA must remove override-only fields owned by the operator.
	require.NoError(t, c.Delete(ctx, overrides))
	require.Eventually(t, func() bool {
		return c.Get(ctx, key(ds.Name), ds) == nil && ds.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("1")) == 0
	}, 20*time.Second, 100*time.Millisecond)

	pod = ds.Spec.Template.Spec
	require.Empty(t, pod.NodeSelector)
	require.Empty(t, pod.Tolerations)
	require.Empty(t, pod.Containers[0].Resources.Limits)
	require.False(t, slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "fabric" }))
	require.False(t, slices.ContainsFunc(pod.Containers[0].Env, func(v corev1.EnvVar) bool { return v.Name == "RACER_ENABLE_RDMA" }))
	require.NoError(t, c.Get(ctx, key("racer-dataplane-config"), config))
	require.Equal(t, "2", config.Data["RACER_MAX_THREADS"])
}

func integrationRacerWriteScope(t *testing.T, rc *rest.Config, admin client.Client, namespace string) {
	t.Helper()

	config := rest.CopyConfig(rc)
	config.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":racer-controller", Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + namespace}}
	c, err := client.New(config, client.Options{Scheme: admin.Scheme()})
	require.NoError(t, err)
	ctx := t.Context()
	// Wait for the API server's policy informer to enforce the persisted binding.
	deadline := time.Now().Add(30 * time.Second)

	for {
		probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "admin-denied", Namespace: namespace}}

		denial := c.Create(ctx, probe, client.DryRunAll)
		if apierrors.IsForbidden(denial) && strings.Contains(denial.Error(), "Racer may only write") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("admission never enforced policy: %T %v forbidden=%v", denial, denial, apierrors.IsForbidden(denial))
		}

		time.Sleep(100 * time.Millisecond)
	}

	for _, name := range []string{"racer-config", "racer-dataplane-config", override.ConfigMapName, "arbitrary-admin-config"} {
		obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(obj), obj); apierrors.IsNotFound(err) {
			require.NoError(t, admin.Create(ctx, obj))
		} else {
			require.NoError(t, err)
		}

		base := obj.DeepCopy()
		obj.Data = map[string]string{"unsafe": "write"}
		require.True(t, apierrors.IsForbidden(c.Update(ctx, obj)), name)
		require.True(t, apierrors.IsForbidden(c.Patch(ctx, obj, client.MergeFrom(base))), name)
		obj.ResourceVersion, obj.UID = "", ""
		err := c.Create(ctx, obj, client.DryRunAll)
		require.True(t, apierrors.IsForbidden(err), "%s: %v", name, err)
	}

	for _, name := range []string{"racer-controller-tls", "admin-secret"} {
		obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		require.True(t, apierrors.IsForbidden(c.Create(ctx, obj, client.DryRunAll)))
	}

	for _, name := range []string{"racer-issuer", "racer-keyring"} {
		obj := &corev1.Secret{}
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj))
		require.NoError(t, c.Update(ctx, obj, client.DryRunAll))
	}

	version := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer-version"}, version))
	require.NoError(t, c.Update(ctx, version, client.DryRunAll))

	ds := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer-dataplane"}, ds))
	require.True(t, apierrors.IsForbidden(c.Update(ctx, ds, client.DryRunAll)))
	require.True(t, apierrors.IsForbidden(c.Delete(ctx, ds, client.DryRunAll)))
	ds.ResourceVersion, ds.UID = "", ""
	require.True(t, apierrors.IsForbidden(c.Create(ctx, ds, client.DryRunAll)))
}
