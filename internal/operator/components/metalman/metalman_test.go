// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metalman

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func TestEnabled(t *testing.T) {
	if (Component{}).Enabled(&unboundedv1alpha3.Site{}) {
		t.Fatal("metalman enabled with no component spec")
	}

	enabled := true
	site := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Metalman: &unboundedv1alpha3.MetalmanComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}},
	}}}

	if !(Component{}).Enabled(site) {
		t.Fatal("metalman not enabled when spec enables it")
	}
}

func TestMutateSupportObject(t *testing.T) {
	keep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}
	if err := mutateSupportObject(keep); err != nil {
		t.Fatalf("mutateSupportObject returned error: %v", err)
	}

	if keep.Object == nil {
		t.Fatalf("metalman support object was dropped")
	}

	drop := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]any{"name": "machina-controller"},
	}}
	if err := mutateSupportObject(drop); err != nil {
		t.Fatalf("mutateSupportObject returned error: %v", err)
	}

	if drop.Object != nil {
		t.Fatalf("non-metalman object was not dropped")
	}
}

func TestDeployment(t *testing.T) {
	enabled := true
	dhcpAuto := true
	replicas := int32(3)
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			DHCPAutoInterface: &dhcpAuto,
			Replicas:          &replicas,
		}}},
	}

	d := deployment(site, component.DefaultNamespace, component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3", APIServerEndpoint: "https://api.example:6443"})
	if d.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", d.Name)
	}

	if d.Namespace != component.DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", d.Namespace, component.DefaultNamespace)
	}

	container := d.Spec.Template.Spec.Containers[0]
	if container.Image != "registry.example.com/metalman:v1.2.3" {
		t.Fatalf("image = %q", container.Image)
	}

	if got := container.Args; len(got) != 3 || got[0] != "serve-pxe" || got[1] != "--site=rack-a" || got[2] != "--dhcp-auto-interface" {
		t.Fatalf("args = %#v", got)
	}

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 3 {
		t.Fatalf("replicas = %v, want 3", d.Spec.Replicas)
	}

	for _, path := range []string{"deployment", "selector", "pod"} {
		var got string

		switch path {
		case "deployment":
			got = d.Labels[unboundedv1alpha3.MachineSiteLabelKey]
		case "selector":
			got = d.Spec.Selector.MatchLabels[unboundedv1alpha3.MachineSiteLabelKey]
		case "pod":
			got = d.Spec.Template.Labels[unboundedv1alpha3.MachineSiteLabelKey]
		}

		if got != "rack-a" {
			t.Fatalf("%s site label = %q, want rack-a", path, got)
		}
	}

	podNS := findEnv(container.Env, "POD_NAMESPACE")
	if podNS == nil || podNS.ValueFrom == nil || podNS.ValueFrom.FieldRef == nil ||
		podNS.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Fatalf("POD_NAMESPACE env = %#v, want Downward API metadata.namespace", podNS)
	}

	if got := findEnv(container.Env, "METALMAN_APISERVER_URL"); got == nil || got.Value != "https://api.example:6443" {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want https://api.example:6443", got)
	}

	assertSiteOwnerRef(t, d.OwnerReferences, "rack-a", "site-uid")
	assertSiteAffinity(t, d.Spec.Template.Spec.Affinity, "rack-a")

	strategy := d.Spec.Strategy
	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate strategy, got %+v", strategy)
	}

	if got := strategy.RollingUpdate.MaxSurge; got == nil || got.IntValue() != 0 {
		t.Fatalf("expected maxSurge=0, got %v", got)
	}

	if got := strategy.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 1 {
		t.Fatalf("expected maxUnavailable=1, got %v", got)
	}
}

func TestDeploymentRespectsNamespaceAndDefaults(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	d := deployment(site, "custom-ns", component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"})
	if d.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", d.Namespace)
	}

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want default 1", d.Spec.Replicas)
	}

	if got := findEnv(d.Spec.Template.Spec.Containers[0].Env, "METALMAN_APISERVER_URL"); got != nil {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want unset when APIServerEndpoint is empty", got)
	}
}

func TestDeploymentAllowsZeroReplicas(t *testing.T) {
	enabled := true
	replicas := int32(0)
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			Replicas:          &replicas,
		}}},
	}

	d := deployment(site, component.DefaultNamespace, component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"})
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", d.Spec.Replicas)
	}
}

func TestCleanupDeletesDeployment(t *testing.T) {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "metalman-controller-rack-a", Namespace: component.DefaultNamespace}}
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(deploy).Build(),
		Namespace: component.DefaultNamespace,
	}

	if err := cleanup(t, env, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(deploy), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected Deployment deleted, got err=%v", err)
	}
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}

	return nil
}

func assertSiteOwnerRef(t *testing.T, refs []metav1.OwnerReference, siteName, uid string) {
	t.Helper()

	if len(refs) != 1 {
		t.Fatalf("ownerReferences len = %d, want 1: %#v", len(refs), refs)
	}

	ref := refs[0]
	if ref.APIVersion != unboundedv1alpha3.GroupVersion.String() || ref.Kind != "Site" || ref.Name != siteName {
		t.Fatalf("unexpected ownerRef: %#v", ref)
	}

	if uid != "" && string(ref.UID) != uid {
		t.Fatalf("ownerRef UID = %q, want %q", ref.UID, uid)
	}

	// The reference must be a controller reference; Owns() enqueues only via
	// metav1.GetControllerOf, so a non-controller ref breaks per-site self-heal.
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerRef is not a controller reference: %#v", ref)
	}
}

func assertSiteAffinity(t *testing.T, affinity *corev1.Affinity, siteName string) {
	t.Helper()

	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("missing node affinity: %#v", affinity)
	}

	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node selector terms len = %d, want 2: %#v", len(terms), terms)
	}

	want := map[string]bool{component.SiteLabelKey: false, component.DeprecatedSiteLabelKey: false}

	for _, term := range terms {
		if len(term.MatchExpressions) != 1 {
			t.Fatalf("term must have one expression: %#v", term)
		}

		expr := term.MatchExpressions[0]
		if expr.Operator != corev1.NodeSelectorOpIn || len(expr.Values) != 1 || expr.Values[0] != siteName {
			t.Fatalf("unexpected site affinity expression: %#v", expr)
		}

		if _, ok := want[expr.Key]; !ok {
			t.Fatalf("unexpected site affinity key %q", expr.Key)
		}

		want[expr.Key] = true
	}

	for key, seen := range want {
		if !seen {
			t.Fatalf("site affinity missing key %q", key)
		}
	}
}

// cleanup plans and executes the component's cleanup, mirroring the reconciler.
func cleanup(t *testing.T, env *component.Env, site *unboundedv1alpha3.Site) error {
	t.Helper()

	c := Component{}

	plan, _, err := c.CleanupPlan(t.Context(), env, site)
	if err != nil {
		return err
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return err
	}

	return exec.Err()
}

// reconcile plans and executes the component, mirroring the reconciler.
func reconcile(t *testing.T, env *component.Env, site *unboundedv1alpha3.Site) component.Result {
	t.Helper()

	c := Component{}

	plan, res, err := c.Plan(t.Context(), env, site)
	if err != nil {
		return component.Failed(err)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return component.Failed(err)
	}

	return component.CombineResult(c.Name(), site.Name, res, exec)
}

// TestPlanGolden pins the complete set of operations the metalman component
// plans for one Site.
//
// The support RBAC ships in the machina manifest set and is byte-identical for
// every Site, so every entry carries a shared key and the executor writes it
// once per pass rather than once per Site. Before plan-then-execute this set
// was re-applied in full for every Site on every pass.
//
// The machina component skips exactly these objects (see
// machina.applyMutator), so a change here without a matching change there
// either double-applies or drops RBAC silently.
func TestPlanGolden(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)},
			},
		}},
	}

	plan, res, err := (Component{}).Plan(t.Context(), env, site)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	want := `Apply ServiceAccount/unbounded-system/metalman-controller [shared]
Apply ServiceAccount/unbounded-system/metalman-bootstrap [shared]
Apply Role/unbounded-system/metalman-controller [shared]
Apply RoleBinding/unbounded-system/metalman-controller [shared]
Apply Role/kube-public/metalman-controller [shared]
Apply RoleBinding/kube-public/metalman-controller [shared]
Apply Role/kube-system/metalman-controller [shared]
Apply RoleBinding/kube-system/metalman-controller [shared]
Apply ClusterRole/metalman-controller [shared]
Apply ClusterRoleBinding/metalman-controller [shared]
Apply ClusterRoleBinding/metalman:node-bootstrapper [shared]
Apply ClusterRoleBinding/metalman:node-autoapprove-bootstrap [shared]
Apply ClusterRole/metalman-bootstrap [shared]
Apply ClusterRoleBinding/metalman:bootstrap-machine [shared]
Apply Deployment/unbounded-system/metalman-controller-rack-a [overridable]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestCleanupPlanGolden pins what disabling metalman for a Site removes. The
// shared RBAC is deliberately left behind: it is harmless unreferenced and may
// still be in use by another Site.
func TestCleanupPlanGolden(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	plan, res, err := (Component{}).CleanupPlan(t.Context(), env,
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}})
	if err != nil {
		t.Fatalf("CleanupPlan: %v", err)
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want ready with reason %q", res, component.ReasonDisabled)
	}

	want := "Delete Deployment/unbounded-system/metalman-controller-rack-a\n"

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestReconcileAppliesPlannedObjects exercises the full plan-then-execute path
// and asserts the executor writes exactly what the plan described, so a plan
// that looks right in TestPlanGolden cannot silently fail to reach the cluster.
func TestReconcileAppliesPlannedObjects(t *testing.T) {
	applied := map[string]bool{}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[named.GetKind()+"/"+named.GetName()] = true

				return nil
			},
		}).
		Build()

	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)},
			},
		}},
	}

	if res := reconcile(t, env, site); !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	for _, want := range []string{
		"Deployment/metalman-controller-rack-a",
		"ServiceAccount/metalman-controller",
		"ClusterRole/metalman-controller",
	} {
		if !applied[want] {
			t.Fatalf("%s was planned but never applied", want)
		}
	}
}
