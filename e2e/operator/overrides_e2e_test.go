//go:build e2e

// This file holds a kind-based e2e for component workload overrides.
//
// It exists because the properties that matter most cannot be tested with the
// fake client. Server-side apply ownership, managedFields, what happens when
// the operator stops declaring a field, and whether the apiserver accepts a
// merged object at all are real apiserver behaviour; the fake client's Apply is
// a stub and its validation is nearly nonexistent.
//
// The whole SiteReconciler runs in-process against a real API server, in the
// same style as the reaper e2e, so no image build is needed. Driving the real
// reconciler rather than a reimplementation of it is the point: an earlier
// version of this file rebuilt a simplified copy of the reconcile pass, which
// meant the parts most likely to be wrong (the failure model, status
// publication, execution order) were tested only against a copy that could
// drift from the original and did.
//
// Run via `go test -tags=e2e -run TestOverrides ./e2e/operator/...`.
package operatore2e

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

const (
	overridesClusterName = "operator-overrides-e2e"

	// overridesNamespace is deliberately not created by the test. The operator
	// is the only thing that creates it, so every subtest also proves that the
	// namespace is written before anything that lands in it.
	overridesNamespace = "unbounded-system"

	// overridesOrderingNamespace is written by a component that plans its
	// contents first and the namespace last, so only kind-inferred ordering
	// makes the pass succeed.
	overridesOrderingNamespace = "unbounded-ordering-e2e"

	overrideSite     = "rack-a"
	overrideWorkload = "override-e2e-agent"
	overrideConfig   = "override-e2e-config"
	overrideAccount  = "override-e2e-agent"
)

// overrideTestComponent plans a ConfigMap and an overridable DaemonSet,
// standing in for any of the real components.
//
// The operations are emitted in the wrong order on purpose, with no DependsOn
// between them, so a pass that does not order by kind would apply a DaemonSet
// into a namespace that does not exist yet. That failure is invisible against
// the fake client and immediate against a real apiserver.
type overrideTestComponent struct{}

func (overrideTestComponent) Name() string          { return "net" }
func (overrideTestComponent) ConditionType() string { return "NetReady" }

func (c overrideTestComponent) Plan(
	_ context.Context, env *component.Env, _ []unboundedv1alpha3.Site,
) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	plan.Add(
		component.Operation{
			Kind:        component.OpApply,
			Object:      overrideTestWorkload(env.Namespace),
			Component:   c.Name(),
			Overridable: true,
		},
		component.Operation{
			Kind:      component.OpApply,
			Object:    overrideTestConfigMap(env.Namespace),
			Component: c.Name(),
		},
		component.Operation{
			Kind:      component.OpApply,
			Object:    overrideTestServiceAccount(env.Namespace),
			Component: c.Name(),
		},
	)

	return plan, component.Reconciled(), nil
}

// overrideTestServiceAccount mirrors the component ServiceAccounts the operator
// really applies: labels, no annotations. The absence of annotations is the
// whole point, so it is asserted rather than assumed.
func overrideTestServiceAccount(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":      overrideAccount,
			"namespace": namespace,
			"labels":    map[string]any{"app.kubernetes.io/name": "override-e2e"},
		},
	}}
}

func overrideTestWorkload(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": overrideWorkload, "namespace": namespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "override-e2e"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "override-e2e"}},
				"spec": map[string]any{
					"volumes": []any{
						map[string]any{"name": "host-run", "hostPath": map[string]any{"path": "/run"}},
					},
					"containers": []any{
						map[string]any{
							"name":  "agent",
							"image": "registry.k8s.io/pause:3.10",
							"args":  []any{"--operator-owned"},
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "10m"},
							},
							"volumeMounts": []any{
								map[string]any{"name": "host-run", "mountPath": "/run"},
							},
						},
					},
				},
			},
		},
	}}
}

func overrideTestConfigMap(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": overrideConfig, "namespace": namespace},
		"data":       map[string]any{"payload": "operator-owned"},
	}}
}

// overrideTestSite is the minimum Site the CRD will accept. Its networking
// fields are required and irrelevant here: nothing in this suite reads them.
func overrideTestSite() *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: overrideSite},
		Spec: unboundedv1alpha3.SiteSpec{
			NodeCidrs: []string{"10.10.0.0/16"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{CidrBlocks: []string{"10.20.0.0/16"}},
			},
		},
	}
}

// orderingTestComponent plans a namespace last and its contents first, with no
// DependsOn anywhere.
//
// This is the arrangement that made ordering a per-component responsibility
// worth removing. Against the fake client, applying a DaemonSet into a
// namespace that does not exist succeeds silently. Against a real apiserver it
// fails with NotFound, so this component only reconciles if the executor
// infers the order from the kinds involved.
type orderingTestComponent struct{}

func (orderingTestComponent) Name() string          { return "ordering" }
func (orderingTestComponent) ConditionType() string { return "OrderingReady" }

func (c orderingTestComponent) Plan(
	_ context.Context, _ *component.Env, _ []unboundedv1alpha3.Site,
) (*component.Plan, component.Result, error) {
	namespace := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": overridesOrderingNamespace},
	}}

	plan := component.NewPlan()
	plan.Add(
		component.Operation{
			Kind:      component.OpApply,
			Object:    overrideTestWorkload(overridesOrderingNamespace),
			Component: c.Name(),
		},
		component.Operation{
			Kind:      component.OpApply,
			Object:    overrideTestConfigMap(overridesOrderingNamespace),
			Component: c.Name(),
		},
		component.Operation{Kind: component.OpApply, Object: namespace, Component: c.Name()},
	)

	return plan, component.Reconciled(), nil
}

// TestOverridesAgainstRealAPIServer covers the behaviour only a real apiserver
// exhibits, in one cluster to keep the suite's runtime reasonable.
func TestOverridesAgainstRealAPIServer(t *testing.T) {
	requireBins(t, "kind", "kubectl", "docker")

	kubeconfig := createClusterNamed(t, overridesClusterName)
	repoRoot := repoRootFromWD(t)
	applyCRDs(t, kubeconfig, repoRoot)

	cl := newClient(t, kubeconfig)
	ctx := t.Context()

	// The reconciler logs through controller-runtime, which complains loudly
	// and once per process if no logger was ever installed.
	ctrl.SetLogger(logr.Discard())

	mustCreate(ctx, t, cl, overrideTestSite())

	fixture := &overrideFixture{client: cl, reconciler: &operator.SiteReconciler{
		Client:    cl,
		Scheme:    cl.Scheme(),
		Namespace: overridesNamespace,
		Registry: &component.Registry{
			Cluster: []component.ClusterComponent{overrideTestComponent{}},
		},
	}}

	// Ordered: the first subtest establishes the baseline the rest build on.
	t.Run("the operator creates the namespace it installs into", func(t *testing.T) {
		assertNamespaceIsCreatedBeforeItsContents(ctx, t, fixture)
	})

	t.Run("execution order is inferred from the kind", func(t *testing.T) {
		assertOrderIsInferredFromKind(ctx, t, cl)
	})

	t.Run("override applies and reverts", func(t *testing.T) {
		assertOverrideAppliesAndReverts(ctx, t, fixture)
	})

	t.Run("invalid document leaves the workload untouched", func(t *testing.T) {
		assertInvalidOverrideLeavesWorkloadUntouched(ctx, t, fixture)
	})

	t.Run("a rejected write is reported as degraded", func(t *testing.T) {
		assertRejectedWriteIsNotReportedAsApplied(ctx, t, fixture)
	})

	t.Run("competing field manager survives override removal", func(t *testing.T) {
		assertCompetingManagerSurvives(ctx, t, fixture)
	})

	t.Run("user annotations on a ServiceAccount survive", func(t *testing.T) {
		assertServiceAccountAnnotationsSurvive(ctx, t, fixture)
	})
}

// assertNamespaceIsCreatedBeforeItsContents proves both the single namespace
// owner and the kind-inferred execution order in the only way that is
// convincing: nothing but the operator creates the namespace, and the component
// plans its DaemonSet before anything else with no dependency declared. Against
// a real apiserver an out-of-order pass fails immediately with NotFound.
func assertNamespaceIsCreatedBeforeItsContents(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	var namespace corev1.Namespace

	err := f.client.Get(ctx, client.ObjectKey{Name: overridesNamespace}, &namespace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the namespace must not exist before the first pass (err = %v)", err)
	}

	f.reconcile(ctx, t)

	if err := f.client.Get(ctx, client.ObjectKey{Name: overridesNamespace}, &namespace); err != nil {
		t.Fatalf("the operator must create the namespace it installs into: %v", err)
	}

	// Both objects landed, which they could not have done had the DaemonSet
	// been applied in the order the component emitted it.
	f.getDaemonSet(ctx, t)

	var configMap corev1.ConfigMap
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: overridesNamespace, Name: overrideConfig}, &configMap); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
}

// assertOrderIsInferredFromKind proves the executor orders by kind rather than
// relying on components to declare their dependencies.
//
// The component plans its DaemonSet and ConfigMap before the namespace they
// live in, with nothing declared between them. A real apiserver rejects a write
// into a namespace that does not exist, so the pass only succeeds if the
// namespace was hoisted ahead of them.
func assertOrderIsInferredFromKind(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()

	reconciler := &operator.SiteReconciler{
		Client:    cl,
		Scheme:    cl.Scheme(),
		Namespace: overridesNamespace,
		Registry: &component.Registry{
			Cluster: []component.ClusterComponent{orderingTestComponent{}},
		},
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Name: overrideSite},
	}); err != nil {
		t.Fatalf("a component that declares no ordering must still reconcile: %v", err)
	}

	var workload appsv1.DaemonSet

	key := client.ObjectKey{Namespace: overridesOrderingNamespace, Name: overrideWorkload}
	if err := cl.Get(ctx, key, &workload); err != nil {
		t.Fatalf("get DaemonSet %s: %v", key, err)
	}
}

// assertOverrideAppliesAndReverts is the revert-scope test.
//
// Removing an override must restore the operator's own value, which works only
// because the operator declares the full object on every apply and therefore
// owns those fields under server-side apply. That is real apiserver behaviour,
// and asserting it against managedFields is the whole reason this runs on kind.
func assertOverrideAppliesAndReverts(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)

	baseline := f.getDaemonSet(ctx, t)
	if got := baseline.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "10m" {
		t.Fatalf("baseline cpu request = %q, want the operator's 10m", got)
	}

	// The operator must own the field it declares, or reverting could not work.
	assertFieldOwner(t, baseline, component.FieldOwner)

	f.writeOverrides(ctx, t, `
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: agent
                resources:
                  requests:
                    cpu: 250m
`)

	f.reconcile(ctx, t)

	overridden := f.getDaemonSet(ctx, t)
	if got := overridden.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "250m" {
		t.Fatalf("overridden cpu request = %q, want 250m", got)
	}

	if overridden.Annotations[override.HashAnnotation] == "" {
		t.Fatal("an overridden workload must carry its override hash")
	}

	// The Site reports what actually reached the cluster, and the hashes are
	// only comparable because both come from the same pass.
	status := f.overrideStatus(ctx, t)
	if status.Phase != unboundedv1alpha3.OverridePhaseApplied {
		t.Fatalf("phase = %q, want Applied; status = %+v", status.Phase, status)
	}

	if len(status.Workloads) != 1 {
		t.Fatalf("workloads = %+v, want the one overridden DaemonSet", status.Workloads)
	}

	if status.Workloads[0].AppliedHash != overridden.Annotations[override.HashAnnotation] {
		t.Fatalf("status applied hash %q does not match the object's %q",
			status.Workloads[0].AppliedHash, overridden.Annotations[override.HashAnnotation])
	}

	// Removing the override must restore the operator's value rather than leave
	// the user's behind.
	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)

	reverted := f.getDaemonSet(ctx, t)
	if got := reverted.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "10m" {
		t.Fatalf("reverted cpu request = %q, want the operator's 10m restored", got)
	}

	if reverted.Annotations[override.HashAnnotation] != "" {
		t.Fatalf("override hash %q survived removal", reverted.Annotations[override.HashAnnotation])
	}

	if phase := f.overrideStatus(ctx, t).Phase; phase != unboundedv1alpha3.OverridePhaseNone {
		t.Fatalf("phase = %q, want None once the document is gone", phase)
	}
}

// assertInvalidOverrideLeavesWorkloadUntouched is the failure-model test.
//
// An unusable document must leave the running workload exactly as it is rather
// than reverting it to defaults, because reverting rewrites running
// infrastructure over a user's mistake. The objects overrides cannot target
// must still reconcile, so one typo does not stop the operator doing its other
// work.
func assertInvalidOverrideLeavesWorkloadUntouched(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	// Establish a known-good override first, so there is something to lose.
	f.writeOverrides(ctx, t, `
  - component: net
    kind: DaemonSet
    extraArgs:
      agent: ["--from-override"]
`)

	f.reconcile(ctx, t)

	before := f.getDaemonSet(ctx, t)

	args := before.Spec.Template.Spec.Containers[0].Args
	if len(args) != 2 || args[1] != "--from-override" {
		t.Fatalf("setup failed: args = %v", args)
	}

	// Drift the ConfigMap, so the pass has something non-overridable to fix.
	f.driftConfigMap(ctx, t)

	// Break the document.
	f.writeOverrides(ctx, t, `
  - component: net
    kind: DaemonSet
    patch:
      metadata:
        name: renamed
`)

	if err := f.reconcileExpectingError(ctx, t); err == nil {
		t.Fatal("an invalid document must fail the pass so it requeues")
	}

	after := f.getDaemonSet(ctx, t)

	gotArgs := after.Spec.Template.Spec.Containers[0].Args
	if len(gotArgs) != 2 || gotArgs[1] != "--from-override" {
		t.Fatalf("args = %v, want the previous override left in place; an invalid document must not revert running workloads", gotArgs)
	}

	if after.Annotations[override.HashAnnotation] != before.Annotations[override.HashAnnotation] {
		t.Fatal("an invalid document must not change the workload at all")
	}

	// The ConfigMap is not overridable, so it reconciled despite the broken
	// document.
	var configMap corev1.ConfigMap
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: overridesNamespace, Name: overrideConfig}, &configMap); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}

	if configMap.Data["payload"] != "operator-owned" {
		t.Fatalf("payload = %q, want drift corrected; a broken document must not stop objects overrides cannot target",
			configMap.Data["payload"])
	}

	status := f.overrideStatus(ctx, t)
	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", status.Phase)
	}

	if status.Message == "" {
		t.Fatal("a degraded status must explain itself")
	}

	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)
}

// assertRejectedWriteIsNotReportedAsApplied is the end-to-end form of the
// status defect.
//
// The document below is valid by every rule the operator enforces: the path is
// allowlisted, the container exists, the value is a string. Only the apiserver
// knows that "banana" is not a quantity. Applied hashes used to be read from
// the plan, so the Site reported Applied for an override the apiserver had
// refused outright, which is the one case the status exists to catch. Only a
// real apiserver rejects it, so only this test can catch the regression.
func assertRejectedWriteIsNotReportedAsApplied(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)

	f.writeOverrides(ctx, t, `
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: agent
                resources:
                  requests:
                    cpu: banana
`)

	if err := f.reconcileExpectingError(ctx, t); err == nil {
		t.Fatal("the apiserver must reject the merged object and the pass must report it")
	}

	status := f.overrideStatus(ctx, t)
	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded for an override the apiserver refused", status.Phase)
	}

	for _, workload := range status.Workloads {
		if workload.AppliedHash != "" {
			t.Fatalf("workload %s reports applied hash %q for a write the apiserver rejected",
				workload.Name, workload.AppliedHash)
		}
	}

	// The workload itself is untouched, still carrying the operator's value.
	got := f.getDaemonSet(ctx, t)
	if cpu := got.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); cpu != "10m" {
		t.Fatalf("cpu = %q, want the operator's value; a rejected write must change nothing", cpu)
	}

	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)
}

// assertCompetingManagerSurvives documents the limit of the revert guarantee.
//
// Server-side apply only reclaims fields the operator still declares and still
// owns. A field written by another manager survives override removal, which is
// correct and is why the design scopes the guarantee rather than promising a
// blanket revert.
func assertCompetingManagerSurvives(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	f.deleteOverrides(ctx, t)
	f.reconcile(ctx, t)

	// Another controller adds a label the operator never declares.
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      overrideWorkload,
			"namespace": overridesNamespace,
			"labels":    map[string]any{"owned-by": "another-controller"},
		},
	}}

	if err := f.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(foreign),
		client.FieldOwner("another-controller"), client.ForceOwnership); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}

	f.reconcile(ctx, t)

	got := f.getDaemonSet(ctx, t)
	if got.Labels["owned-by"] != "another-controller" {
		t.Fatalf("labels = %v, want the competing manager's field preserved; the operator only reclaims what it declares", got.Labels)
	}
}

// assertServiceAccountAnnotationsSurvive covers workload identity.
//
// Every cloud's workload identity integration works by annotating the
// ServiceAccount: azure.workload.identity/client-id, eks.amazonaws.com/role-arn,
// iam.gke.io/gcp-service-account. Overrides cannot reach ServiceAccounts, which
// are not Deployments or DaemonSets, so whether this works at all rests
// entirely on server-side apply semantics.
//
// It does work, and for a precise reason: the operator declares labels on its
// ServiceAccounts and never declares annotations, so it owns no key in that
// map. ForceOwnership resolves conflicts on declared fields; it cannot remove a
// field the applier never mentions.
//
// That reasoning is exactly the kind that is right until someone adds an
// annotation to a component manifest, so it is asserted against a real
// apiserver rather than left as a comment. The documentation makes a promise to
// users on the strength of this test.
func assertServiceAccountAnnotationsSurvive(ctx context.Context, t *testing.T, f *overrideFixture) {
	t.Helper()

	f.reconcile(ctx, t)

	key := client.ObjectKey{Namespace: overridesNamespace, Name: overrideAccount}

	var account corev1.ServiceAccount
	if err := f.client.Get(ctx, key, &account); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}

	// The operator must not be declaring annotations, or the guarantee below
	// rests on nothing.
	if len(account.Annotations) != 0 {
		t.Fatalf("the operator declares annotations %v on its ServiceAccount; "+
			"user annotations are only safe while it declares none", account.Annotations)
	}

	// A user annotates it, as any workload identity setup instructs.
	if account.Annotations == nil {
		account.Annotations = map[string]string{}
	}

	account.Annotations["azure.workload.identity/client-id"] = "00000000-0000-0000-0000-000000000000"

	if err := f.client.Update(ctx, &account); err != nil {
		t.Fatalf("annotate ServiceAccount: %v", err)
	}

	// Several passes, because a single one could pass by luck of timing.
	for range 3 {
		f.reconcile(ctx, t)
	}

	var after corev1.ServiceAccount
	if err := f.client.Get(ctx, key, &after); err != nil {
		t.Fatalf("get ServiceAccount after reconcile: %v", err)
	}

	if after.Annotations["azure.workload.identity/client-id"] == "" {
		t.Fatalf("the operator removed a user annotation it never declared; "+
			"annotations = %v", after.Annotations)
	}

	// The operator's own field is still reconciled, so this is not simply the
	// operator having stopped managing the object.
	if after.Labels["app.kubernetes.io/name"] != "override-e2e" {
		t.Fatalf("labels = %v, want the operator's own field still owned", after.Labels)
	}
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// overrideFixture drives the real SiteReconciler against a real cluster.
type overrideFixture struct {
	client     client.Client
	reconciler *operator.SiteReconciler
}

// reconcile runs one pass of the real reconciler and fails on error.
func (f *overrideFixture) reconcile(ctx context.Context, t *testing.T) {
	t.Helper()

	if err := f.reconcileExpectingError(ctx, t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// reconcileExpectingError runs one pass and returns its error, so tests can
// assert on the failure paths.
func (f *overrideFixture) reconcileExpectingError(ctx context.Context, t *testing.T) error {
	t.Helper()

	_, err := f.reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Name: overrideSite},
	})

	return err
}

// overrideStatus returns the Site's published override status.
func (f *overrideFixture) overrideStatus(ctx context.Context, t *testing.T) *unboundedv1alpha3.OverrideStatus {
	t.Helper()

	var site unboundedv1alpha3.Site
	if err := f.client.Get(ctx, client.ObjectKey{Name: overrideSite}, &site); err != nil {
		t.Fatalf("get Site: %v", err)
	}

	if site.Status.Overrides == nil {
		t.Fatal("the Site published no override status at all")
	}

	return site.Status.Overrides
}

// writeOverrides installs an overrides document from a list fragment.
func (f *overrideFixture) writeOverrides(ctx context.Context, t *testing.T, overrides string) {
	t.Helper()

	data := map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion + "\noverrides:" + overrides,
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: overridesNamespace, Name: override.ConfigMapName},
		Data:       data,
	}

	var existing corev1.ConfigMap

	err := f.client.Get(ctx, client.ObjectKeyFromObject(configMap), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if createErr := f.client.Create(ctx, configMap); createErr != nil {
			t.Fatalf("create overrides: %v", createErr)
		}
	case err != nil:
		t.Fatalf("get overrides: %v", err)
	default:
		existing.Data = data
		if updateErr := f.client.Update(ctx, &existing); updateErr != nil {
			t.Fatalf("update overrides: %v", updateErr)
		}
	}
}

func (f *overrideFixture) deleteOverrides(ctx context.Context, t *testing.T) {
	t.Helper()

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: overridesNamespace, Name: override.ConfigMapName},
	}

	if err := f.client.Delete(ctx, configMap); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete overrides: %v", err)
	}
}

// driftConfigMap edits an object overrides can never target, so a test can show
// that a broken document does not stop it being reconciled.
func (f *overrideFixture) driftConfigMap(ctx context.Context, t *testing.T) {
	t.Helper()

	var configMap corev1.ConfigMap

	key := client.ObjectKey{Namespace: overridesNamespace, Name: overrideConfig}
	if err := f.client.Get(ctx, key, &configMap); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}

	configMap.Data = map[string]string{"payload": "drifted"}
	if err := f.client.Update(ctx, &configMap); err != nil {
		t.Fatalf("drift ConfigMap: %v", err)
	}
}

func (f *overrideFixture) getDaemonSet(ctx context.Context, t *testing.T) *appsv1.DaemonSet {
	t.Helper()

	var got appsv1.DaemonSet

	key := client.ObjectKey{Namespace: overridesNamespace, Name: overrideWorkload}

	if err := utilwait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := f.client.Get(ctx, key, &got); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}

				return false, err
			}

			return true, nil
		}); err != nil {
		t.Fatalf("get DaemonSet %s: %v", key, err)
	}

	return &got
}

// assertFieldOwner proves the operator owns what it declares, which is the
// mechanism the revert guarantee rests on.
func assertFieldOwner(t *testing.T, obj *appsv1.DaemonSet, owner string) {
	t.Helper()

	for _, entry := range obj.ManagedFields {
		if entry.Manager == owner {
			return
		}
	}

	t.Fatalf("no managedFields entry for %q; the operator must own the fields it declares (%+v)", owner, obj.ManagedFields)
}
