// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMutateStorageScopesDaemonSetToSite(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "run"}}},
			},
		},
	}}

	if err := mutateStorageObject(site, obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
	}

	if got := obj.GetName(); got != "unbounded-storage-supervisor-rack-a" {
		t.Fatalf("name = %q, want unbounded-storage-supervisor-rack-a", got)
	}

	if got := obj.GetLabels()[siteLabelKey]; got != "rack-a" {
		t.Fatalf("metadata site label = %q, want rack-a", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
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

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing storage affinity: ok=%t err=%v", ok, err)
	}

	assertSiteAffinityMap(t, affinity, "rack-a")
}

func TestMutateMachinaSkipsMetalmanSupport(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}

	r := &SiteReconciler{}
	if err := r.mutateMachinaObject(obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	if obj.Object != nil {
		t.Fatalf("metalman support object was not skipped")
	}
}

func TestMutateMachinaInjectsAPIServerEndpoint(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "machina-config"},
		"data":       map[string]any{"config.yaml": `apiServerEndpoint: ""`},
	}}

	r := &SiteReconciler{Config: Config{APIServerEndpoint: "https://api.example:6443"}}
	if err := r.mutateMachinaObject(obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	got, ok, err := unstructured.NestedString(obj.Object, "data", "config.yaml")
	if err != nil || !ok {
		t.Fatalf("missing data.config.yaml: ok=%t err=%v", ok, err)
	}

	if got != `apiServerEndpoint: "https://api.example:6443"` {
		t.Fatalf("config.yaml = %q", got)
	}
}

func TestMutateMetalmanSupportObject(t *testing.T) {
	keep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}
	if err := mutateMetalmanSupportObject(keep); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if keep.Object == nil {
		t.Fatalf("metalman support object was dropped")
	}

	drop := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]any{"name": "machina-controller"},
	}}
	if err := mutateMetalmanSupportObject(drop); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if drop.Object != nil {
		t.Fatalf("non-metalman object was not dropped")
	}
}

func TestMetalmanDeployment(t *testing.T) {
	enabled := true
	dhcpAuto := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			DHCPAutoInterface: &dhcpAuto,
		}}},
	}

	deployment := metalmanDeployment(site, DefaultNamespace, Config{MetalmanImage: "example/metalman:default"})
	if deployment.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", deployment.Name)
	}

	if deployment.Namespace != DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", deployment.Namespace, DefaultNamespace)
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "example/metalman:default" {
		t.Fatalf("image = %q", container.Image)
	}

	if got := container.Args; len(got) != 3 || got[0] != "serve-pxe" || got[1] != "--site=rack-a" || got[2] != "--dhcp-auto-interface" {
		t.Fatalf("args = %#v", got)
	}

	assertSiteOwnerRef(t, deployment.OwnerReferences, "rack-a", "site-uid")
	assertSiteAffinity(t, deployment.Spec.Template.Spec.Affinity, "rack-a")

	// hostNetwork singleton: must terminate the old pod before creating the new
	// one so it can rebind its host ports on a rolling restart.
	strategy := deployment.Spec.Strategy
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

func TestMetalmanDeploymentRespectsNamespace(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	deployment := metalmanDeployment(site, "custom-ns", Config{MetalmanImage: "example/metalman:default"})
	if deployment.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", deployment.Namespace)
	}
}

func TestReconcilerNamespaceFallsBackToDefault(t *testing.T) {
	r := &SiteReconciler{}
	if got := r.namespace(); got != DefaultNamespace {
		t.Fatalf("namespace() = %q, want %q", got, DefaultNamespace)
	}

	r.Namespace = "custom-ns"
	if got := r.namespace(); got != "custom-ns" {
		t.Fatalf("namespace() = %q, want custom-ns", got)
	}
}

func TestStorageDaemonSetPointsAtPerSiteConfig(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "run"}},
					"volumes": []any{map[string]any{
						"name":      "config-source",
						"configMap": map[string]any{"name": "unbounded-storage-config"},
					}},
				},
			},
		},
	}}

	if err := mutateStorageObject(site, obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
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

func TestEnsureStorageConfigCreatesDefaultWhenAbsent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	if err := r.ensureStorageConfig(t.Context(), site); err != nil {
		t.Fatalf("ensureStorageConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: DefaultNamespace, Name: "unbounded-storage-config-rack-a"}, &got); err != nil {
		t.Fatalf("get created configmap: %v", err)
	}

	if got.Data["config.yaml"] == "" {
		t.Fatalf("default config.yaml was not seeded")
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestEnsureStorageConfigAdoptsExistingAndPreservesData(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true"},
	}
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	if err := r.ensureStorageConfig(t.Context(), site); err != nil {
		t.Fatalf("ensureStorageConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get adopted configmap: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" {
		t.Fatalf("existing config data was not preserved: %q", got.Data["config.yaml"])
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestReconcilePerSiteComponentDisabledRunsCleanup(t *testing.T) {
	cleaned := false
	status := (&SiteReconciler{}).reconcilePerSiteComponent(
		false,
		func() error { t.Fatal("reconcile must not run when disabled"); return nil },
		func() error { cleaned = true; return nil },
	)

	if !cleaned {
		t.Fatal("cleanup was not called for a disabled component")
	}

	if !status.ready || status.reason != reasonDisabled {
		t.Fatalf("status = %+v, want ready with reason %q", status, reasonDisabled)
	}
}

func TestCleanupStorageDeletesPerSiteResourcesOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: DefaultNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: DefaultNamespace}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-b", Namespace: DefaultNamespace}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds, cm, otherDS).Build()
	r := &SiteReconciler{Client: cl}

	if err := r.cleanupStorage(t.Context(), &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("cleanupStorage: %v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a DaemonSet deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a ConfigMap deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(otherDS), &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("expected rack-b DaemonSet preserved, got err=%v", err)
	}
}

func TestRetargetNamespaceRewritesToCustomNamespace(t *testing.T) {
	r := &SiteReconciler{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "x", "namespace": "unbounded-system"},
		"subjects":   []any{map[string]any{"namespace": "unbounded-system"}},
	}}

	r.retargetNamespace(obj)

	if got := obj.GetNamespace(); got != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", got)
	}

	subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
	if subjects[0].(map[string]any)["namespace"] != "custom-ns" {
		t.Fatalf("subject namespace not rewritten: %v", subjects[0])
	}

	// Default install is a no-op.
	def := &SiteReconciler{}
	nsObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "unbounded-system"},
	}}
	def.retargetNamespace(nsObj)

	if nsObj.GetName() != "unbounded-system" {
		t.Fatalf("default install rewrote namespace to %q", nsObj.GetName())
	}
}

// Finding 4: retargeting a custom namespace must also rewrite the namespace
// where it appears as a substring - service-account usernames in VAP CEL and
// flag values - while leaving unrelated strings (image refs) untouched.
func TestRetargetNamespaceRewritesServiceAccountAndArgs(t *testing.T) {
	r := &SiteReconciler{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   map[string]any{"name": "p"},
		"spec": map[string]any{
			"matchConditions": []any{
				map[string]any{"expression": "request.userInfo.username == 'system:serviceaccount:unbounded-system:unbounded-net-controller'"},
			},
			"args":  []any{"--leader-elect-resource-namespace=unbounded-system"},
			"image": "ghcr.io/azure/unbounded-system:v1",
		},
	}}

	r.retargetNamespace(obj)

	conds, _, _ := unstructured.NestedSlice(obj.Object, "spec", "matchConditions")
	gotExpr := conds[0].(map[string]any)["expression"].(string)

	if gotExpr != "request.userInfo.username == 'system:serviceaccount:custom-ns:unbounded-net-controller'" {
		t.Fatalf("SA username not rewritten: %q", gotExpr)
	}

	args, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "args")
	if len(args) != 1 || args[0] != "--leader-elect-resource-namespace=custom-ns" {
		t.Fatalf("flag value not rewritten: %v", args)
	}

	if img, _, _ := unstructured.NestedString(obj.Object, "spec", "image"); img != "ghcr.io/azure/unbounded-system:v1" {
		t.Fatalf("image ref must not be rewritten, got %q", img)
	}
}

func TestRetargetNamespaceInString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"unbounded-system", "custom-ns"},
		{"system:serviceaccount:unbounded-system:sa", "system:serviceaccount:custom-ns:sa"},
		{"--leader-elect-resource-namespace=unbounded-system", "--leader-elect-resource-namespace=custom-ns"},
		{"ghcr.io/azure/unbounded-system:v1", "ghcr.io/azure/unbounded-system:v1"},
		{"unbounded-system-other", "unbounded-system-other"},
	}

	for _, tc := range cases {
		if got := retargetNamespaceInString(tc.in, "unbounded-system", "custom-ns"); got != tc.want {
			t.Fatalf("retargetNamespaceInString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Finding 3: the reconcile loop never applies CRDs (the operator installs them
// once at startup via BootstrapCRDs).
func TestSkipCRDObjects(t *testing.T) {
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "sites.unbounded-cloud.io"},
	}}

	if err := skipCRDObjects(crd); err != nil {
		t.Fatalf("skipCRDObjects: %v", err)
	}

	if crd.Object != nil {
		t.Fatal("expected CRD object nilled out")
	}

	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"},
	}}

	if err := skipCRDObjects(other); err != nil {
		t.Fatalf("skipCRDObjects: %v", err)
	}

	if other.Object == nil {
		t.Fatal("non-CRD object must be preserved")
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

	want := map[string]bool{siteLabelKey: false, deprecatedSiteLabelKey: false}

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

func assertSiteAffinityMap(t *testing.T, affinity map[string]any, siteName string) {
	t.Helper()

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	assertSiteAffinity(t, converted, siteName)
}

func TestApplyObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1 to scheme: %v", err)
	}

	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}}},
		},
	}

	if err := r.applyObject(t.Context(), deployment); err != nil {
		t.Fatalf("applyObject returned error: %v", err)
	}
}
