// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	netapi "github.com/Azure/unbounded/api/net/v1alpha1"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/gantry"
	"github.com/Azure/unbounded/internal/operator/components/racer"
)

type transitionWriteTransport struct {
	http.RoundTripper
	writes []string
}

func (r *transitionWriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.RoundTripper.RoundTrip(req)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && req.Method != http.MethodGet {
		r.writes = append(r.writes, req.Method+" "+req.URL.Path)
	}

	return resp, err
}

// Exercise the merged Gantry/Racer plan through the real API. Comparing rendered
// plans or fake-client objects cannot prove SSA prunes formerly owned fields.
func TestGantryRacerAPITransitions(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real server-side apply")
	}

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../deploy/machina/crd", "../../deploy/racer/crd"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := newReconcilerTestScheme(t)
	for _, add := range []func(*runtime.Scheme) error{coordinationv1.AddToScheme, rbacv1.AddToScheme, schedulingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	observed := &transitionWriteTransport{}
	operatorConfig := rest.CopyConfig(cfg)
	operatorConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		observed.RoundTripper = rt
		return observed
	})

	operatorClient, err := client.New(operatorConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	const namespace = "gantry-transition"

	reconciler := &SiteReconciler{
		Client: operatorClient, APIReader: operatorClient, Scheme: scheme, Namespace: namespace,
		Config:   Config{ImageRegistry: "example.test", ImageTag: "transition-test"},
		Registry: &component.Registry{Cluster: []component.ClusterComponent{gantry.New(), racer.NewControlPlane(), racer.NewDataplane()}},
	}

	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}, Spec: machina.SiteSpec{
		NodeCidrs: []string{"10.0.0.0/24"}, PodCidrAssignments: []netapi.PodCidrAssignment{{CidrBlocks: []string{"10.1.0.0/16"}}},
	}}
	if err := kube.Create(t.Context(), site); err != nil {
		t.Fatal(err)
	}

	run := func() {
		t.Helper()

		result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
		if err != nil {
			t.Fatal(err)
		}

		if result.RequeueAfter != 0 {
			t.Errorf("reconcile requested another pass without a concurrent writer: %+v", result)
		}
	}
	get := func(obj client.Object, name string) {
		t.Helper()

		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
			t.Fatal(err)
		}
	}
	quiet := func() {
		t.Helper()

		observed.writes = nil

		run()

		if len(observed.writes) != 0 {
			t.Errorf("converged pass wrote to the API: %v", observed.writes)
		}
	}

	run()

	var direct appsv1.DaemonSet
	get(&direct, "gantry")

	if !slices.Contains(direct.Spec.Template.Spec.Containers[0].Args, "--content-backend=direct") {
		t.Fatal("initial installation did not select direct")
	}

	quiet()

	cache := &racerapi.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "backing", Annotations: map[string]string{"unbounded-cloud.io/gantry-backing": "true"}}}

	cache.Spec = racerapi.P2PCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3}
	if err := kube.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	assertRacer := func() appsv1.DaemonSet {
		t.Helper()
		run()

		var ds appsv1.DaemonSet
		get(&ds, "gantry")

		pod := ds.Spec.Template.Spec
		if ds.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-uid"] != string(cache.UID) ||
			!slices.Contains(pod.Containers[0].Args, "--content-backend=racer") || !slices.Contains(pod.Containers[0].Args, "--racer-cache-name=backing") {
			t.Fatalf("Racer selection did not reach stored pod: %+v", ds.Spec.Template)
		}

		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.SecurityContext == nil || !slices.Contains(pod.SecurityContext.SupplementalGroups, int64(65532)) {
			t.Fatal("Racer socket security settings missing")
		}

		if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
			return v.Name == "racer-sockets" && v.HostPath != nil && v.HostPath.Path == "/dev/racer"
		}) ||
			!slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(v corev1.VolumeMount) bool { return v.Name == "racer-sockets" }) ||
			!strings.Contains(strings.Join(pod.InitContainers[0].Command, " "), "/dev/racer/backing") {
			t.Fatal("Racer socket volume, mount, or init command missing")
		}

		for _, port := range pod.Containers[0].Ports {
			if port.Name == "transfer" || port.Name == "chaircall" {
				t.Fatalf("SSA retained direct-only port %s", port.Name)
			}
		}

		get(&appsv1.Deployment{}, "racer-controlplane")
		get(&appsv1.DaemonSet{}, "racer-dataplane")
		quiet()

		return ds
	}
	first := assertRacer()

	var retained appsv1.DaemonSet
	get(&retained, "racer-dataplane")

	assertDirect := func() {
		t.Helper()
		run()

		var ds appsv1.DaemonSet
		get(&ds, "gantry")
		// Exact stored baseline equality covers removal of sockets, init mounts
		// and commands, supplemental groups, token override and UID annotation,
		// as well as restoration of the direct-only ports and arguments.
		if !reflect.DeepEqual(ds.Spec.Template, direct.Spec.Template) {
			t.Errorf("SSA failed to restore direct pod template (-want +got):\n%s", cmp.Diff(direct.Spec.Template, ds.Spec.Template))
		}

		var role rbacv1.Role
		get(&role, "gantry-agent")

		if !slices.ContainsFunc(role.Rules, func(rule rbacv1.PolicyRule) bool {
			return slices.Contains(rule.Resources, "leases") && slices.Contains(rule.Verbs, "update")
		}) {
			t.Fatal("direct chair permissions were not restored")
		}

		var binding rbacv1.RoleBinding
		get(&binding, "gantry-agent")

		if binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != "gantry" {
			t.Fatal("direct chair binding was not restored")
		}

		var leases coordinationv1.LeaseList
		if err := kube.List(t.Context(), &leases, client.InNamespace(namespace)); err != nil || len(leases.Items) != 64 {
			t.Fatalf("chair leases=%d, error=%v", len(leases.Items), err)
		}

		var current appsv1.DaemonSet
		get(&current, "racer-dataplane")

		if current.UID != retained.UID || current.ResourceVersion != retained.ResourceVersion {
			t.Fatal("backing transition rolled retained Racer dataplane")
		}

		quiet()
	}
	// Simulate absent rollback prerequisites while Racer is active. The merged
	// plan must restore them before switching Gantry back to direct.
	for _, obj := range []client.Object{&rbacv1.Role{}, &rbacv1.RoleBinding{}, &coordinationv1.Lease{}} {
		name := "gantry-agent"
		if _, ok := obj.(*coordinationv1.Lease); ok {
			name = "gantry-chair-00"
		}

		get(obj, name)

		if err := kube.Delete(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	quiet()
	delete(cache.Annotations, "unbounded-cloud.io/gantry-backing")

	if err := kube.Update(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	assertDirect()

	cache.Annotations = map[string]string{"unbounded-cloud.io/gantry-backing": "true"}
	if err := kube.Update(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	assertRacer()

	if err := kube.Delete(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	assertDirect()

	oldUID := cache.UID
	cache = &racerapi.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "backing", Annotations: map[string]string{"unbounded-cloud.io/gantry-backing": "true"}}}

	cache.Spec = racerapi.P2PCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3}
	if err := kube.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	if cache.UID == oldUID {
		t.Fatal("API reused deleted cache UID")
	}

	replacement := assertRacer()
	if replacement.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-uid"] == first.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-uid"] {
		t.Fatal("same-name cache replacement did not change pod rollout identity")
	}

	var live racerapi.P2PCache
	if err := kube.Get(t.Context(), client.ObjectKey{Name: cache.Name}, &live); err != nil {
		t.Fatal(err)
	}

	if live.ResourceVersion != cache.ResourceVersion {
		t.Fatal("operator wrote user-owned P2PCache")
	}
}
