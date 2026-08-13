// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace: component.DefaultNamespace,
	}
}

func TestEnabled(t *testing.T) {
	if (Component{}).Enabled(&unboundedv1alpha3.Site{}) {
		t.Fatal("storage enabled with no component spec")
	}

	enabled := true
	site := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Storage: &unboundedv1alpha3.StorageComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}},
	}}}

	if !(Component{}).Enabled(site) {
		t.Fatal("storage not enabled when spec enables it")
	}
}

func TestMutateObjectScopesDaemonSetToSite(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
				"spec": map[string]any{
					"initContainers": []any{map[string]any{"name": "install", "image": "old:init"}},
					"containers":     []any{map[string]any{"name": "run", "image": "old:run"}},
				},
			},
		},
	}}

	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}
	if err := mutateObject(site, cfg, "storage-hash", obj); err != nil {
		t.Fatalf("mutateObject returned error: %v", err)
	}

	if got := obj.GetName(); got != "unbounded-storage-supervisor-rack-a" {
		t.Fatalf("name = %q, want unbounded-storage-supervisor-rack-a", got)
	}

	if got := obj.GetLabels()[component.SiteLabelKey]; got != "rack-a" {
		t.Fatalf("metadata site label = %q, want rack-a", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", component.SiteLabelKey},
		{"spec", "template", "metadata", "labels", component.SiteLabelKey},
	} {
		got, ok, err := unstructured.NestedString(obj.Object, path...)
		if err != nil || !ok {
			t.Fatalf("missing %v: ok=%t err=%v", path, ok, err)
		}

		if got != "rack-a" {
			t.Fatalf("%v = %q, want rack-a", path, got)
		}
	}

	assertSiteOwnerRef(t, obj.GetOwnerReferences(), "rack-a", "site-uid")

	annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if annotations[ConfigHashAnnotation] != "storage-hash" {
		t.Fatalf("storage config hash annotation = %q, want storage-hash", annotations[ConfigHashAnnotation])
	}

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing storage affinity: ok=%t err=%v", ok, err)
	}

	assertSiteAffinityMap(t, affinity, "rack-a")

	for _, field := range []string{"initContainers", "containers"} {
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/unbounded-storage-supervisor:v1.2.3" {
			t.Fatalf("%s image = %q", field, got)
		}
	}
}

func TestDaemonSetPointsAtPerSiteConfig(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "run"}},
					"volumes": []any{map[string]any{
						"name":      "config-source",
						"configMap": map[string]any{"name": configName},
					}},
				},
			},
		},
	}}

	if err := mutateObject(site, component.Config{}, "storage-hash", obj); err != nil {
		t.Fatalf("mutateObject returned error: %v", err)
	}

	volumes, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil || !ok {
		t.Fatalf("missing volumes: ok=%t err=%v", ok, err)
	}

	vol := volumes[0].(map[string]any)
	cm := vol["configMap"].(map[string]any)

	if cm["name"] != "unbounded-storage-config-rack-a" {
		t.Fatalf("config volume name = %v, want unbounded-storage-config-rack-a", cm["name"])
	}
}

func TestEnsureConfigCreatesDefaultWhenAbsent(t *testing.T) {
	env := testEnv(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := ensureConfig(t, env, site)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: "unbounded-storage-config-rack-a"}, &got); err != nil {
		t.Fatalf("get created configmap: %v", err)
	}

	if got.Data["config.yaml"] == "" {
		t.Fatalf("default config.yaml was not seeded")
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact stored payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestEnsureConfigAdoptsExistingAndPreservesData(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true"},
	}
	env := testEnv(t, existing)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := ensureConfig(t, env, site)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get adopted configmap: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" {
		t.Fatalf("existing config data was not preserved: %q", got.Data["config.yaml"])
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact adopted payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestCleanupDeletesPerSiteResourcesOnly(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: component.DefaultNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: component.DefaultNamespace}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-b", Namespace: component.DefaultNamespace}}

	env := testEnv(t, ds, cm, otherDS)

	if err := cleanup(t, env, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a DaemonSet deleted, got err=%v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a ConfigMap deleted, got err=%v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(otherDS), &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("expected rack-b DaemonSet preserved, got err=%v", err)
	}
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

func assertSiteAffinityMap(t *testing.T, affinity map[string]any, siteName string) {
	t.Helper()

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	if converted.NodeAffinity == nil || converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("missing node affinity: %#v", converted)
	}

	terms := converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node selector terms len = %d, want 2", len(terms))
	}

	want := map[string]bool{component.SiteLabelKey: false, component.DeprecatedSiteLabelKey: false}

	for _, term := range terms {
		expr := term.MatchExpressions[0]
		if len(expr.Values) != 1 || expr.Values[0] != siteName {
			t.Fatalf("unexpected site affinity expression: %#v", expr)
		}

		want[expr.Key] = true
	}

	for key, seen := range want {
		if !seen {
			t.Fatalf("site affinity missing key %q", key)
		}
	}
}

// ensureConfig plans and executes the per-site config operation, mirroring what
// the reconciler does so these tests exercise the production path, including
// owner-reference adoption of an existing payload.
func ensureConfig(t *testing.T, env *component.Env, site *unboundedv1alpha3.Site) (string, error) {
	t.Helper()

	hash, op, err := planConfig(t.Context(), env, site)
	if err != nil {
		return "", err
	}

	if op == nil {
		return hash, nil
	}

	plan := component.NewPlan()
	plan.Add(*op)

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return "", err
	}

	return hash, exec.Err()
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

// TestPlanGolden pins the complete set of operations the storage component
// plans for one Site.
//
// This is the safety net for the plan-then-execute conversion and for anything
// that later changes what storage writes. The reaper gates its migration on the
// per-site DaemonSet name and the config-hash annotation it carries
// (internal/operator/migrate.go), so an object silently appearing,
// disappearing, or being renamed here breaks the upgrade path rather than
// failing a test.
//
// The namespace and RBAC are shared across Sites and carry a shared key so the
// executor writes them once per pass rather than once per Site.
func TestPlanGolden(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "uid-a"}}
	env := testEnv(t)

	plan, res, err := (Component{}).Plan(t.Context(), env, site)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	want := `CreateIfAbsent ConfigMap/unbounded-system/unbounded-storage-config-rack-a
Apply ServiceAccount/unbounded-system/unbounded-storage-supervisor [shared]
Apply ClusterRole/unbounded-storage-supervisor [shared]
Apply ClusterRoleBinding/unbounded-storage-supervisor [shared]
Apply DaemonSet/unbounded-system/unbounded-storage-supervisor-rack-a [overridable] [after ConfigMap/unbounded-system/unbounded-storage-config-rack-a]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestCleanupPlanGolden pins what disabling storage for a Site removes. It must
// stay scoped to the per-Site resources; the shared namespace and RBAC belong
// to every Site.
func TestCleanupPlanGolden(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}

	plan, res, err := (Component{}).CleanupPlan(t.Context(), testEnv(t), site)
	if err != nil {
		t.Fatalf("CleanupPlan: %v", err)
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want ready with reason %q", res, component.ReasonDisabled)
	}

	want := `Delete DaemonSet/unbounded-system/unbounded-storage-supervisor-rack-a
Delete ConfigMap/unbounded-system/unbounded-storage-config-rack-a
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestReconcileAppliesPlannedObjects exercises the full plan-then-execute path
// and asserts the executor writes exactly what the plan described, including
// creating the per-site ConfigMap before the DaemonSet that carries its hash.
func TestReconcileAppliesPlannedObjects(t *testing.T) {
	applied := map[string]bool{}

	scheme := testEnv(t).Scheme

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
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

	env := &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "uid-a"}}

	if res := reconcile(t, env, site); !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	if !applied["DaemonSet/unbounded-storage-supervisor-rack-a"] {
		t.Fatal("per-site DaemonSet was planned but never applied")
	}

	// The create-if-absent config must have reached the cluster too.
	var cm corev1.ConfigMap
	if err := cl.Get(t.Context(), client.ObjectKey{
		Namespace: component.DefaultNamespace,
		Name:      "unbounded-storage-config-rack-a",
	}, &cm); err != nil {
		t.Fatalf("per-site ConfigMap was planned but never created: %v", err)
	}
}

// TestExecutionOrderGolden pins what the executor runs, as distinct from what
// the component emits. Storage shares its ServiceAccount and RBAC across every
// Site, so those are written once ahead of any per-Site workload.
func TestExecutionOrderGolden(t *testing.T) {
	env := testEnv(t)

	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "uid-a"}}

	plan, _, err := (Component{}).Plan(t.Context(), env, site)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	got, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	want := `Apply ServiceAccount/unbounded-system/unbounded-storage-supervisor
Apply ClusterRole/unbounded-storage-supervisor
Apply ClusterRoleBinding/unbounded-storage-supervisor
CreateIfAbsent ConfigMap/unbounded-system/unbounded-storage-config-rack-a
Apply DaemonSet/unbounded-system/unbounded-storage-supervisor-rack-a
`

	if got != want {
		t.Fatalf("execution order =\n%s\nwant\n%s", got, want)
	}
}
