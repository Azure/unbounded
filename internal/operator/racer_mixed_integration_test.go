// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racercomponent "github.com/Azure/unbounded/internal/operator/components/racer"
	"github.com/Azure/unbounded/internal/operator/override"
	racercore "github.com/Azure/unbounded/internal/racer"
)

// Real API/SSA and production generic reconciliation, with explicit controller
// acknowledgements: envtest has no DaemonSet controller, scheduler or kubelet.
// Three actual Nodes/Pods exercise occupancy; 1,500 virtual Nodes evaluate the
// persisted Kubernetes affinity, including all OR terms and base constraints.
func TestEnvtestRacerMixedMigration(t *testing.T) {
	heartbeat, stopHeartbeat := context.WithCancel(t.Context())
	defer stopHeartbeat()

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-heartbeat.Done():
				return
			case <-ticker.C:
				t.Log("heartbeat: TestEnvtestRacerMixedMigration; see last stage checkpoint")
			}
		}
	}()

	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server migration")
	}
	// BootstrapCRDs embeds rendered machina/net CRDs; fail immediately when
	// this worktree has not run the manifest prerequisites.
	embedded := map[string]bool{}

	for _, fsys := range bootstrapManifestSets() {
		files, err := component.YamlFiles(fsys)
		require.NoError(t, err)

		for _, file := range files {
			embedded[file] = true
		}
	}

	require.True(t, embedded["crd/unbounded-cloud.io_sites.yaml"], "run make machina-manifests net-manifests before this integration test")

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	t.Log("checkpoint: envtest API started")

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, machinav1.AddToScheme, racerv1.AddToScheme} {
		require.NoError(t, add(scheme))
	}

	c, err := client.New(rc, client.Options{Scheme: scheme})
	require.NoError(t, err)

	ctx, stop := context.WithTimeout(t.Context(), 100*time.Second)
	defer stop()

	require.NoError(t, BootstrapCRDs(ctx, c))
	t.Log("checkpoint: CRDs bootstrapped")

	const ns = "racer-mixed"
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	require.NoError(t, c.Create(ctx, &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "mixed"}}))

	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }
	newReconciler := func() *SiteReconciler {
		return &SiteReconciler{Client: c, APIReader: c, Scheme: scheme, Namespace: ns, Config: Config{ImageRegistry: "example.test", ImageTag: "v1"}, Registry: &component.Registry{Cluster: []component.ClusterComponent{racercomponent.New()}}}
	}

	pass := func() { _, err := newReconciler().Reconcile(ctx, singletonRequest()); require.NoError(t, err) }
	for range 4 {
		pass()
	}

	job := &batchv1.Job{}
	require.NoError(t, c.Get(ctx, key("racer-initialize"), job))
	require.Equal(t, []string{"initialize"}, job.Spec.Template.Spec.Containers[0].Args)

	config := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, key("racer-config"), config))

	for name, value := range config.Data {
		t.Setenv(name, value)
	}

	t.Setenv("POD_NAMESPACE", ns)

	cfg, err := racercore.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, racercore.Assemble(cfg, c, c).Topology.InitializeVersion(ctx))
	t.Log("checkpoint: initialized")

	config.Data["RACER_HOST_NETWORK"] = "true"
	require.NoError(t, c.Update(ctx, config))
	// A legacy kind-only host guard must survive mixed-mode SSA and must never
	// land on podnet. Two user OR terms exercise all-term intersection.
	document := `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: racer
    kind: DaemonSet
    addInitContainers: [underlay-guard]
    patch:
      spec:
        template:
          spec:
            initContainers:
              - name: underlay-guard
                image: example.test/guard:v1
                securityContext:
                  runAsUser: 0
                  capabilities:
                    add: [NET_ADMIN]
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: test-zone
                          operator: In
                          values: [a]
                    - matchExpressions:
                        - key: test-zone
                          operator: In
                          values: [b]
  - component: racer
    kind: DaemonSet
    name: racer-dataplane-podnet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: dataplane
                env:
                  - name: MIXED_TEST
                    value: podnet
`
	require.NoError(t, c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: override.ConfigMapName, Namespace: ns}, Data: map[string]string{"mixed.yaml": document}}))

	get := func(name string) *appsv1.DaemonSet {
		ds := &appsv1.DaemonSet{}
		require.NoError(t, c.Get(ctx, key(name), ds))

		return ds
	}

	const (
		hostName = "racer-dataplane"
		podName  = racercore.PodNetworkDaemonSetName
	)

	ack := func(name string) {
		ds := get(name)
		ds.Status.ObservedGeneration = ds.Generation
		require.NoError(t, c.Status().Update(ctx, ds))
	}

	pass()

	host := get(hostName)

	t.Log("checkpoint: C0 host workload applied")

	hostUID, hostSelector := host.UID, host.Spec.Selector.DeepCopy()

	ack(hostName)

	match := func(ds *appsv1.DaemonSet, index int) bool {
		return mixedMatches(t, ds, fmt.Sprintf("node-%04d", index), map[string]string{"kubernetes.io/os": "linux", "test-zone": "a"})
	}
	occupant := func(ds *appsv1.DaemonSet, index int) *corev1.Pod {
		node := fmt.Sprintf("node-%04d", index)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", ds.Name, index), Namespace: ns, Labels: ds.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}, Finalizers: []string{"test.unbounded-cloud.io/drain"}}, Spec: *ds.Spec.Template.Spec.DeepCopy()}
		p.Spec.NodeName = node
		require.NoError(t, c.Create(ctx, p))

		return p
	}

	for _, i := range []int{0, 1, 20} {
		require.NoError(t, c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%04d", i)}}))
	}

	sources := []*corev1.Pod{occupant(host, 0), occupant(host, 1)}
	stable := occupant(host, 20)
	stableUID := stable.UID
	// Configure exactly 11 exceptions without creating 1,500 API objects.
	require.NoError(t, c.Get(ctx, key("racer-config"), config))
	config.Data["RACER_POD_NETWORK_NODES"] = `["node-0000","node-0001","node-0002","node-0003","node-0004","node-0005","node-0006","node-0007","node-0008","node-0009","node-0010"]`
	require.NoError(t, c.Update(ctx, config))
	pass()

	podUID := get(podName).UID

	for range 2 {
		pass() // fresh reconciler each pass, including before source observation
		require.False(t, match(get(podName), 0))
		require.False(t, match(get(podName), 2), "empty source node is not admitted before drain observation")
		require.True(t, match(get(hostName), 20), "observation lag must not drain unrelated host nodes")
	}

	ack(hostName)
	ack(podName)
	// An unscheduled source still blocks destination admission. It can be
	// created after the scheduling template changed, before observation.
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending-source", Namespace: ns, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(get(hostName), appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: *host.Spec.Template.Spec.DeepCopy()}
	require.NoError(t, c.Create(ctx, pending))
	pass()
	require.False(t, match(get(podName), 2))
	require.NoError(t, c.Delete(ctx, pending, client.GracePeriodSeconds(0)))
	pass()

	for _, p := range sources {
		require.NoError(t, c.Delete(ctx, p, client.GracePeriodSeconds(0)))
	}

	pass()
	require.False(t, match(get(podName), 0), "terminating source must block")

	for _, p := range sources {
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(p), p))
		p.Finalizers = nil
		require.NoError(t, c.Update(ctx, p))
	}

	for range 3 {
		ack(hostName)
		ack(podName)
		pass()
	}

	assertSteady := func(mixed bool) {
		h, p := get(hostName), get(podName)
		require.Equal(t, hostUID, h.UID)
		require.Equal(t, podUID, p.UID)
		require.Equal(t, hostSelector, h.Spec.Selector)
		require.Len(t, h.Spec.Template.Spec.InitContainers, 1)
		require.Empty(t, p.Spec.Template.Spec.InitContainers)
		require.True(t, h.Spec.Template.Spec.HostNetwork)
		require.False(t, p.Spec.Template.Spec.HostNetwork)
		require.Equal(t, h.Spec.Template.Spec.ServiceAccountName, p.Spec.Template.Spec.ServiceAccountName)
		require.Equal(t, h.Spec.Template.Spec.Containers[0].SecurityContext, p.Spec.Template.Spec.Containers[0].SecurityContext)

		hc, pc := 0, 0

		for i := 0; i < 1500; i++ {
			hm, pm := match(h, i), match(p, i)
			require.False(t, hm && pm, "overlap on %d", i)

			if hm {
				hc++
			}

			if pm {
				pc++
			}
		}

		for _, ds := range []*appsv1.DaemonSet{h, p} {
			require.False(t, mixedMatches(t, ds, "node-0020", map[string]string{"kubernetes.io/os": "windows", "test-zone": "b"}))
			require.False(t, mixedMatches(t, ds, "node-0000", map[string]string{"kubernetes.io/os": "linux", "racer.unbounded-cloud.io/exclude": "true", "test-zone": "b"}))
		}

		if mixed {
			require.Equal(t, 1489, hc)
			require.Equal(t, 11, pc)
		} else {
			require.Equal(t, 1500, hc)
			require.Zero(t, pc)
		}

		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(stable), stable))
		require.Equal(t, stableUID, stable.UID)
		// Stable resourceVersions prove repeated apply does not change a template.
		versions := []string{h.ResourceVersion, p.ResourceVersion}

		pass()
		pass()
		require.Equal(t, versions, []string{get(hostName).ResourceVersion, get(podName).ResourceVersion})
	}
	assertSteady(true)
	t.Log("checkpoint: 11 pod-network / 1489 host-network steady")

	hostSelectorMatch, err := metav1.LabelSelectorAsSelector(get(hostName).Spec.Selector)
	require.NoError(t, err)
	require.False(t, hostSelectorMatch.Matches(labels.Set(get(podName).Spec.Template.Labels)))
	// Installed RBAC permits controller reads of both fixed workloads but never
	// workload writes. These are real impersonated API requests, not fake RBAC.
	controllerConfig := rest.CopyConfig(rc)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + ns + ":racer-controller", Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + ns}}
	controllerClient, err := client.New(controllerConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	for _, name := range []string{hostName, podName} {
		ds := &appsv1.DaemonSet{}
		require.NoError(t, controllerClient.Get(ctx, key(name), ds))
		require.True(t, apierrors.IsForbidden(controllerClient.Update(ctx, ds, client.DryRunAll)))
	}
	// Both selectors are disjoint, and the explicit name override reaches podnet.
	require.Contains(t, get(podName).Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "MIXED_TEST", Value: "podnet"})
	reverse := occupant(get(podName), 0)

	require.NoError(t, c.Get(ctx, key("racer-config"), config))
	delete(config.Data, "RACER_POD_NETWORK_NODES")
	require.NoError(t, c.Update(ctx, config))
	pass()
	ack(hostName)
	ack(podName)
	pass()
	require.False(t, match(get(hostName), 0))
	require.NoError(t, c.Delete(ctx, reverse, client.GracePeriodSeconds(0)))
	pass()
	require.False(t, match(get(hostName), 0))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(reverse), reverse))
	reverse.Finalizers = nil
	require.NoError(t, c.Update(ctx, reverse))

	for range 3 {
		ack(hostName)
		ack(podName)
		pass()
	}

	assertSteady(false)
	t.Log("checkpoint: reverse migration steady")
	// Setup and real informer startup must accept both workload watches. A
	// podnet metadata drift event must repair that workload without manual calls.
	mgr, err := ctrl.NewManager(rc, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	require.NoError(t, err)

	r := newReconciler()
	r.Client = mgr.GetClient()
	r.APIReader = mgr.GetAPIReader()
	require.NoError(t, r.SetupWithManager(mgr))

	managerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() { done <- mgr.Start(managerCtx) }()

	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.True(t, mgr.GetCache().WaitForCacheSync(managerCtx))

	p := get(podName)
	base := p.DeepCopy()
	p.Labels["unbounded-cloud.io/test-drift"] = "repair"
	require.NoError(t, c.Patch(ctx, p, client.MergeFrom(base)))
	// Probe a managed field rather than expect SSA to remove an admin field.
	base = p.DeepCopy()
	p.Spec.Template.Spec.Containers[0].Image = "example.test/drift:v0"
	require.NoError(t, c.Patch(ctx, p, client.MergeFrom(base)))
	require.Eventually(t, func() bool {
		ds := &appsv1.DaemonSet{}
		return c.Get(ctx, key(podName), ds) == nil && ds.Spec.Template.Spec.Containers[0].Image == "example.test/racer-dataplane:v1"
	}, 20*time.Second, 100*time.Millisecond)
}

func mixedMatches(t *testing.T, ds *appsv1.DaemonSet, name string, nodeLabels map[string]string) bool {
	t.Helper()

	if !labels.SelectorFromSet(ds.Spec.Template.Spec.NodeSelector).Matches(labels.Set(nodeLabels)) {
		return false
	}

	a := ds.Spec.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}

	for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}

		allowed := true

		for _, r := range term.MatchExpressions {
			op := map[corev1.NodeSelectorOperator]selection.Operator{corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn, corev1.NodeSelectorOpExists: selection.Exists, corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist, corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan}[r.Operator]
			req, err := labels.NewRequirement(r.Key, op, r.Values)
			require.NoError(t, err)

			allowed = allowed && req.Matches(labels.Set(nodeLabels))
		}

		for _, r := range term.MatchFields {
			require.Equal(t, "metadata.name", r.Key)

			switch r.Operator {
			case corev1.NodeSelectorOpIn:
				allowed = allowed && slices.Contains(r.Values, name)
			case corev1.NodeSelectorOpNotIn:
				allowed = allowed && !slices.Contains(r.Values, name)
			default:
				t.Fatalf("unsupported field operator %s", r.Operator)
			}
		}

		if allowed {
			return true
		}
	}

	return false
}
