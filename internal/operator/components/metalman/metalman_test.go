// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{admissionv1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme, unboundedv1alpha3.AddToScheme} {
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
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(deploy).
			WithInterceptorFuncs(interceptor.Funcs{
				// The fake client cannot apply admission objects; the
				// support set is covered by the upgrade tests.
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					return nil
				},
			}).Build(),
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

// cleanup plans and executes one pass over site with metalman disabled,
// mirroring the reconciler.
func cleanup(t *testing.T, env *component.Env, site *unboundedv1alpha3.Site) error {
	t.Helper()

	exec, err := env.Execute(t.Context(), passPlan(t, env, site))
	if err != nil {
		return err
	}

	return exec.Err()
}

// reconcile plans and executes one pass over site, mirroring the reconciler,
// and returns the first verdict that is not ready across the bootstrap and
// per-Site components, or the per-Site verdict when both are ready.
func reconcile(t *testing.T, env *component.Env, site *unboundedv1alpha3.Site) component.Result {
	t.Helper()

	results, plan, err := planPass(t, env, site)
	if err != nil {
		return component.Failed(err)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return component.Failed(err)
	}

	var res component.Result

	for _, name := range []string{BootstrapComponent{}.Name(), Component{}.Name()} {
		res = component.CombineResult(name, site.Name, results[name], exec)
		if !res.Ready {
			return res
		}
	}

	return res
}

// TestPlanGolden pins the complete set of operations the metalman component
// plans for one Site: only the per-Site Deployment, ordered after the
// admission restriction that BootstrapComponent plans once per pass.
func TestPlanGolden(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	plan, res, err := (Component{}).Plan(t.Context(), env, enabledSite())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	want := `Apply Deployment/unbounded-system/metalman-controller-rack-a [overridable] [after ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestBootstrapPlanGolden pins the shared support set BootstrapComponent plans
// once per pass while Sites exist.
//
// The machina component skips exactly these objects (see
// machina.applyMutator), so a change here without a matching change there
// either double-applies or drops RBAC silently.
//
// The cluster is fresh, so the admission restriction is not live yet and the
// bootstrap grant is withheld until a later pass reads it back.
func TestBootstrapPlanGolden(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	plan, res, err := (BootstrapComponent{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*enabledSite(), *disabledSite("rack-b")})
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
Apply ClusterRole/metalman-bootstrap [shared]
Delete ClusterRoleBinding/metalman:bootstrap-machine [shared]
Apply ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction [shared]
Apply ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction [shared]
Delete ClusterRoleBinding/metalman:node-autoapprove-bootstrap [shared]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestCleanupPlanGolden pins what disabling metalman for a Site plans: only
// the per-Site Deployment is removed. The shared support set stays with
// BootstrapComponent, because other Sites may use it and bootstrap tokens
// minted earlier outlive the Deployment.
func TestCleanupPlanGolden(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	plan, res, err := (Component{}).CleanupPlan(t.Context(), env, disabledSite("rack-a"))
	if err != nil {
		t.Fatalf("CleanupPlan: %v", err)
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want ready with reason %q", res, component.ReasonDisabled)
	}

	want := `Delete Deployment/unbounded-system/metalman-controller-rack-a
`

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

// TestReconcileDeletesStaleAutoApproveBinding verifies an upgrade removes the
// ClusterRoleBinding that let kube-controller-manager auto-approve kubelet
// client certificates for any node requested with a metalman-bootstrap token,
// and that the delete is idempotent once the binding is gone.
func TestReconcileDeletesStaleAutoApproveBinding(t *testing.T) {
	stale := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: StaleAutoApproveBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
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

	for range 2 {
		if res := reconcile(t, env, site); !res.Ready {
			t.Fatalf("result = %+v, want ready", res)
		}

		err := cl.Get(t.Context(), client.ObjectKey{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get stale binding: err = %v, want NotFound", err)
		}
	}
}

// TestSupportManifestsDoNotGrantAutoApproval guards against reintroducing the
// nodeclient auto-approval or Machine create for the metalman-bootstrap SA.
func TestSupportManifestsDoNotGrantAutoApproval(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	objs, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, obj := range objs {
		if obj.GetKind() == "ClusterRoleBinding" {
			name, _, _ := unstructured.NestedString(obj.Object, "roleRef", "name")
			if name == "system:certificates.k8s.io:certificatesigningrequests:nodeclient" {
				t.Fatalf("%s binds the nodeclient auto-approval role", obj.GetName())
			}
		}

		if obj.GetKind() == "ClusterRole" && obj.GetName() == "metalman-bootstrap" {
			rules, _, _ := unstructured.NestedSlice(obj.Object, "rules")
			for _, r := range rules {
				rule, _ := r.(map[string]any)
				verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
				resources, _, _ := unstructured.NestedStringSlice(rule, "resources")

				for _, res := range resources {
					for _, v := range verbs {
						if res == "machines" && v != "get" {
							t.Fatalf("metalman-bootstrap grants %q on machines", v)
						}
					}
				}
			}
		}
	}
}

func enabledSite() *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)},
			},
		}},
	}
}

// TestExecutionOrderInstallsRestrictionBeforeBootstrapAccess pins that the
// bootstrap grant and the token-minting Deployment are written only after the
// admission policy and its binding, even though the executor ranks admission
// objects last. The restriction is already live, so the grant is planned.
func TestExecutionOrderInstallsRestrictionBeforeBootstrapAccess(t *testing.T) {
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(liveRestriction(t)...).Build(),
		Namespace: component.DefaultNamespace,
	}

	plan := passPlan(t, env, enabledSite())

	order, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(order), "\n")
	index := func(suffix string) int {
		for i, l := range lines {
			if strings.HasSuffix(l, suffix) {
				return i
			}
		}

		t.Fatalf("%s not in execution order:\n%s", suffix, order)

		return -1
	}

	vap := index("ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction")
	vapb := index("ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction")

	for _, dependent := range []string{
		"ClusterRoleBinding/" + BootstrapMachineBindingName,
		"Deployment/unbounded-system/metalman-controller-rack-a",
	} {
		if i := index(dependent); i < vap || i < vapb {
			t.Fatalf("%s ordered before the admission restriction:\n%s", dependent, order)
		}
	}
}

// TestRestrictionFailureWithholdsBootstrapAccess verifies that when either
// admission object fails to apply, the bootstrap grant and the Deployment are
// not applied.
func TestRestrictionFailureWithholdsBootstrapAccess(t *testing.T) {
	for _, failing := range []string{
		"ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction",
		"ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction",
	} {
		t.Run(failing, func(t *testing.T) {
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

						name := named.GetKind() + "/" + named.GetName()
						if name == failing {
							return errors.New("admission policy rejected")
						}

						applied[name] = true

						return nil
					},
				}).
				Build()

			env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			if res := reconcile(t, env, enabledSite()); res.Ready {
				t.Fatalf("result = %+v, want not ready", res)
			}

			for _, withheld := range []string{
				"ClusterRoleBinding/" + BootstrapMachineBindingName,
				"Deployment/metalman-controller-rack-a",
			} {
				if applied[withheld] {
					t.Fatalf("%s applied although the restriction failed", withheld)
				}
			}

			if !applied["ClusterRole/metalman-controller"] {
				t.Fatal("independent RBAC was not applied; a failure must not abort the pass")
			}
		})
	}
}

// upgradeClient returns a client seeded with the bootstrap grant and the
// Deployment an earlier release installed, plus any extra objects. Applies of
// failing are rejected, other applies succeed without being stored, and
// deletes are counted.
func upgradeClient(t *testing.T, failing string, deletes map[string]int, extra ...client.Object) client.Client {
	t.Helper()

	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: BootstrapMachineBindingName}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: DeploymentName("rack-a"), Namespace: component.DefaultNamespace,
	}}

	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append([]client.Object{crb, dep}, extra...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				if named.GetKind()+"/"+named.GetName() == failing {
					return errors.New("admission policy rejected")
				}

				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes[obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName()]++

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
}

// TestRestrictionFailureRevokesExistingBootstrapAccess verifies that on an
// upgrade, where an earlier release already installed the bootstrap grant, a
// failure to install either admission object deletes the grant rather than
// leaving bootstrap tokens able to write any Machine's status.
func TestRestrictionFailureRevokesExistingBootstrapAccess(t *testing.T) {
	for _, failing := range []string{
		"ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction",
		"ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction",
	} {
		t.Run(failing, func(t *testing.T) {
			deletes := map[string]int{}
			cl := upgradeClient(t, failing, deletes)
			env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			res := reconcile(t, env, enabledSite())
			if res.Ready {
				t.Fatalf("result = %+v, want not ready", res)
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get bootstrap grant = %v, want NotFound; the grant outlived its restriction", err)
			}

			// The Deployment is left running: without the grant its tokens
			// cannot write Machine status, and deleting it would stop PXE
			// service for the Site over an admission install failure.
			err = cl.Get(t.Context(), client.ObjectKey{
				Name: DeploymentName("rack-a"), Namespace: component.DefaultNamespace,
			}, &appsv1.Deployment{})
			if err != nil {
				t.Fatalf("get Deployment: %v, want it left in place", err)
			}
		})
	}
}

// liveRestriction returns the admission policy and binding as the apiserver
// persists them, for tests that start from a cluster where the restriction is
// already live.
func liveRestriction(t *testing.T) []client.Object {
	t.Helper()

	policy, binding := livePolicy(t), liveBinding(t)
	withDefaults(policy, binding)

	return []client.Object{policy, binding}
}

// TestRestrictionSuccessKeepsBootstrapAccess verifies that when the admission
// objects are live, an existing bootstrap grant is applied in place and never
// deleted, so steady-state passes do not interrupt in-flight bootstraps.
func TestRestrictionSuccessKeepsBootstrapAccess(t *testing.T) {
	deletes := map[string]int{}
	cl := upgradeClient(t, "", deletes, liveRestriction(t)...)
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	for pass := range 3 {
		if res := reconcile(t, env, enabledSite()); !res.Ready {
			t.Fatalf("pass %d: result = %+v, want ready", pass, res)
		}
	}

	if n := deletes["ClusterRoleBinding/"+BootstrapMachineBindingName]; n != 0 {
		t.Fatalf("bootstrap grant deleted %d times in the steady state, want 0", n)
	}

	if err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{}); err != nil {
		t.Fatalf("get bootstrap grant: %v", err)
	}
}

// disabledSite returns a Site with metalman not enabled.
func disabledSite(name string) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// planPass builds one reconciler pass over sites: the bootstrap component
// plans once for the pass, then enabled Sites contribute Plan and disabled
// Sites contribute CleanupPlan, merged into one plan. The returned results are
// the planning verdicts by component name; a per-Site verdict is the last
// Site's.
func planPass(t *testing.T, env *component.Env, sites ...*unboundedv1alpha3.Site) (map[string]component.Result, *component.Plan, error) {
	t.Helper()

	all := make([]unboundedv1alpha3.Site, 0, len(sites))
	for _, site := range sites {
		all = append(all, *site)
	}

	results := map[string]component.Result{}

	plan, res, err := BootstrapComponent{}.Plan(t.Context(), env, all)
	if err != nil {
		return nil, nil, err
	}

	results[BootstrapComponent{}.Name()] = res

	c := Component{}

	for _, site := range sites {
		planFn := c.CleanupPlan
		if c.Enabled(site) {
			planFn = c.Plan
		}

		p, res, err := planFn(t.Context(), env, site)
		if err != nil {
			return nil, nil, err
		}

		results[c.Name()] = res

		plan.Merge(p)
	}

	return results, plan, nil
}

// passPlan is planPass for tests that only need the merged plan.
func passPlan(t *testing.T, env *component.Env, sites ...*unboundedv1alpha3.Site) *component.Plan {
	t.Helper()

	_, plan, err := planPass(t, env, sites...)
	if err != nil {
		t.Fatalf("plan pass: %v", err)
	}

	return plan
}

// TestCleanupRestrictsRetainedBootstrapAccess verifies the upgrade where every
// Site has metalman disabled: the bootstrap grant an earlier release installed
// is kept behind the admission restriction, which cleanup installs, and is
// never deleted across passes while the restriction holds.
func TestCleanupRestrictsRetainedBootstrapAccess(t *testing.T) {
	deletes := map[string]int{}
	applied := map[string]int{}
	cl := upgradeClient(t, "", deletes, liveRestriction(t)...)
	cl = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			named := obj.(interface {
				GetKind() string
				GetName() string
			})
			applied[named.GetKind()+"/"+named.GetName()]++

			return c.Apply(ctx, obj, opts...)
		},
	})
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	for pass := range 3 {
		exec, err := env.Execute(t.Context(), passPlan(t, env, disabledSite("rack-a"), disabledSite("rack-b")))
		if err != nil {
			t.Fatalf("pass %d: execute: %v", pass, err)
		}

		if err := exec.Err(); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	for _, name := range []string{
		"ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction",
		"ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction",
	} {
		if applied[name] != 3 {
			t.Fatalf("%s applied %d times, want once per pass", name, applied[name])
		}
	}

	if n := deletes["ClusterRoleBinding/"+BootstrapMachineBindingName]; n != 0 {
		t.Fatalf("bootstrap grant deleted %d times, want 0", n)
	}

	if err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{}); err != nil {
		t.Fatalf("get bootstrap grant: %v", err)
	}

	err := cl.Get(t.Context(), client.ObjectKey{Name: DeploymentName("rack-a"), Namespace: component.DefaultNamespace}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get Deployment = %v, want NotFound", err)
	}
}

// TestCleanupRestrictionFailureRevokesBootstrapAccess verifies that when every
// Site has metalman disabled and the restriction fails to install, cleanup
// revokes the bootstrap grant an earlier release left behind.
func TestCleanupRestrictionFailureRevokesBootstrapAccess(t *testing.T) {
	for _, failing := range []string{
		"ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction",
		"ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction",
	} {
		t.Run(failing, func(t *testing.T) {
			deletes := map[string]int{}
			cl := upgradeClient(t, failing, deletes)
			env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			if err := cleanup(t, env, disabledSite("rack-a")); err == nil {
				t.Fatal("cleanup succeeded, want the restriction failure reported")
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get bootstrap grant = %v, want NotFound; the grant outlived its restriction", err)
			}
		})
	}
}

// TestCleanupDeploymentFailureStillRemovesAutoApproval verifies that a failed
// Deployment delete does not skip the stale auto-approval delete or the
// restriction applies that BootstrapComponent plans in the same pass.
func TestCleanupDeploymentFailureStillRemovesAutoApproval(t *testing.T) {
	stale := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: StaleAutoApproveBindingName}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: DeploymentName("rack-a"), Namespace: component.DefaultNamespace,
	}}
	applied := map[string]int{}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(stale, dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named := obj.(interface {
					GetKind() string
					GetName() string
				})
				applied[named.GetKind()+"/"+named.GetName()]++

				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == DeploymentName("rack-a") {
					return errors.New("delete Deployment failed")
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	if err := cleanup(t, env, disabledSite("rack-a")); err == nil {
		t.Fatal("cleanup succeeded, want the Deployment failure reported")
	}

	err := cl.Get(t.Context(), client.ObjectKey{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get auto-approval binding = %v, want NotFound", err)
	}

	for _, name := range []string{
		"ValidatingAdmissionPolicy/metalman-bootstrap-machine-restriction",
		"ValidatingAdmissionPolicyBinding/metalman-bootstrap-machine-restriction",
	} {
		if applied[name] != 1 {
			t.Fatalf("%s applied %d times, want 1", name, applied[name])
		}
	}
}

// TestMixedSitesShareBootstrapRestriction verifies that a pass over enabled
// and disabled Sites keeps the enabled Site's Deployment and the grant, and
// removes only the disabled Site's Deployment.
func TestMixedSitesShareBootstrapRestriction(t *testing.T) {
	deletes := map[string]int{}
	cl := upgradeClient(t, "", deletes, liveRestriction(t)...)
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	enabled := enabledSite()
	enabled.Name = "rack-b"

	exec, err := env.Execute(t.Context(), passPlan(t, env, disabledSite("rack-a"), enabled))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if err := exec.Err(); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if n := deletes["ClusterRoleBinding/"+BootstrapMachineBindingName]; n != 0 {
		t.Fatalf("bootstrap grant deleted %d times, want 0", n)
	}

	if n := deletes["Deployment/"+DeploymentName("rack-a")]; n != 1 {
		t.Fatalf("disabled Site Deployment deleted %d times, want 1", n)
	}

	if n := deletes["Deployment/"+DeploymentName("rack-b")]; n != 0 {
		t.Fatalf("enabled Site Deployment deleted %d times, want 0", n)
	}
}
