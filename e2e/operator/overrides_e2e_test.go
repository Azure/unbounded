//go:build e2e

// This file holds a kind-based e2e for component workload overrides.
//
// It exists because the properties that matter most cannot be tested with the
// fake client. Server-side apply ownership, managedFields, and what happens
// when the operator stops declaring a field are all real apiserver behaviour;
// the fake client's Apply is a stub. The unit tests assert what the operator
// intends to write, and this asserts what the cluster does with it.
//
// The operator's reconciler runs in-process against a real API server, in the
// same style as the reaper e2e, so no image build is needed.
//
// Run via `go test -tags=e2e -run TestOverrides ./e2e/operator/...`.
package operatore2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

const overridesClusterName = "operator-overrides-e2e"

// overrideTestComponent plans a single overridable DaemonSet, standing in for
// any of the real components. The point of this suite is apiserver behaviour,
// not manifest content, so a minimal workload keeps the assertions legible.
type overrideTestComponent struct{}

func (overrideTestComponent) Name() string          { return "net" }
func (overrideTestComponent) ConditionType() string { return "NetReady" }

func (c overrideTestComponent) Plan(_ context.Context, env *component.Env, _ []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      overrideTestWorkload(env.Namespace),
		Component:   c.Name(),
		Overridable: true,
	})

	return plan, component.Reconciled(), nil
}

func overrideTestWorkload(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "override-e2e-agent", "namespace": namespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "override-e2e"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "override-e2e"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "agent",
							"image": "registry.k8s.io/pause:3.10",
							"args":  []any{"--operator-owned"},
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "10m"},
							},
						},
					},
				},
			},
		},
	}}
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

	namespace := "unbounded-system"
	mustCreate(ctx, t, cl, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})

	t.Run("override applies and reverts", func(t *testing.T) {
		assertOverrideAppliesAndReverts(ctx, t, cl, namespace)
	})

	t.Run("invalid document leaves the workload untouched", func(t *testing.T) {
		assertInvalidOverrideLeavesWorkloadUntouched(ctx, t, cl, namespace)
	})

	t.Run("competing field manager survives override removal", func(t *testing.T) {
		assertCompetingManagerSurvives(ctx, t, cl, namespace)
	})
}

// assertOverrideAppliesAndReverts is the revert-scope test.
//
// Removing an override must restore the operator's own value, which works only
// because the operator declares the full object on every apply and therefore
// owns those fields under server-side apply. That is real apiserver behaviour,
// and asserting it against managedFields is the whole reason this runs on kind.
func assertOverrideAppliesAndReverts(ctx context.Context, t *testing.T, cl client.Client, namespace string) {
	t.Helper()

	deleteOverrides(ctx, t, cl, namespace)
	reconcileOverrides(ctx, t, cl, namespace)

	baseline := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")
	if got := baseline.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "10m" {
		t.Fatalf("baseline cpu request = %q, want the operator's 10m", got)
	}

	// The operator must own the field it declares, or reverting could not work.
	assertFieldOwner(t, baseline, component.FieldOwner)

	writeOverrides(ctx, t, cl, namespace, map[string]string{
		"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
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
`,
	})

	reconcileOverrides(ctx, t, cl, namespace)

	overridden := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")
	if got := overridden.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "250m" {
		t.Fatalf("overridden cpu request = %q, want 250m", got)
	}

	if overridden.Annotations[override.HashAnnotation] == "" {
		t.Fatal("an overridden workload must carry its override hash")
	}

	// Removing the override must restore the operator's value rather than leave
	// the user's behind.
	deleteOverrides(ctx, t, cl, namespace)
	reconcileOverrides(ctx, t, cl, namespace)

	reverted := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")
	if got := reverted.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "10m" {
		t.Fatalf("reverted cpu request = %q, want the operator's 10m restored", got)
	}

	if reverted.Annotations[override.HashAnnotation] != "" {
		t.Fatalf("override hash %q survived removal", reverted.Annotations[override.HashAnnotation])
	}
}

// assertInvalidOverrideLeavesWorkloadUntouched is the failure-model test.
//
// An unusable document must leave the running workload exactly as it is rather
// than reverting it to defaults, because reverting rewrites running
// infrastructure over a user's mistake.
func assertInvalidOverrideLeavesWorkloadUntouched(ctx context.Context, t *testing.T, cl client.Client, namespace string) {
	t.Helper()

	// Establish a known-good override first, so there is something to lose.
	writeOverrides(ctx, t, cl, namespace, map[string]string{
		"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      agent: ["--from-override"]
`,
	})

	reconcileOverrides(ctx, t, cl, namespace)

	before := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")

	args := before.Spec.Template.Spec.Containers[0].Args
	if len(args) != 2 || args[1] != "--from-override" {
		t.Fatalf("setup failed: args = %v", args)
	}

	// Break the document.
	writeOverrides(ctx, t, cl, namespace, map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	})

	if err := reconcileOverridesExpectingError(ctx, cl, namespace); err == nil {
		t.Fatal("an invalid document must fail the pass so it requeues")
	}

	after := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")

	gotArgs := after.Spec.Template.Spec.Containers[0].Args
	if len(gotArgs) != 2 || gotArgs[1] != "--from-override" {
		t.Fatalf("args = %v, want the previous override left in place; an invalid document must not revert running workloads", gotArgs)
	}

	if after.Annotations[override.HashAnnotation] != before.Annotations[override.HashAnnotation] {
		t.Fatal("an invalid document must not change the workload at all")
	}

	deleteOverrides(ctx, t, cl, namespace)
	reconcileOverrides(ctx, t, cl, namespace)
}

// assertCompetingManagerSurvives documents the limit of the revert guarantee.
//
// Server-side apply only reclaims fields the operator still declares and still
// owns. A field written by another manager survives override removal, which is
// correct and is why the design scopes the guarantee rather than promising a
// blanket revert.
func assertCompetingManagerSurvives(ctx context.Context, t *testing.T, cl client.Client, namespace string) {
	t.Helper()

	deleteOverrides(ctx, t, cl, namespace)
	reconcileOverrides(ctx, t, cl, namespace)

	// Another controller adds a label the operator never declares.
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      "override-e2e-agent",
			"namespace": namespace,
			"labels":    map[string]any{"owned-by": "another-controller"},
		},
	}}

	if err := cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(foreign),
		client.FieldOwner("another-controller"), client.ForceOwnership); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}

	reconcileOverrides(ctx, t, cl, namespace)

	got := getDaemonSet(ctx, t, cl, namespace, "override-e2e-agent")
	if got.Labels["owned-by"] != "another-controller" {
		t.Fatalf("labels = %v, want the competing manager's field preserved; the operator only reclaims what it declares", got.Labels)
	}
}

// reconcileOverrides runs one reconcile pass in-process and fails on error.
func reconcileOverrides(ctx context.Context, t *testing.T, cl client.Client, namespace string) {
	t.Helper()

	if err := reconcileOverridesExpectingError(ctx, cl, namespace); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// reconcileOverridesExpectingError runs one pass and returns its error, so
// tests can assert on failure paths.
func reconcileOverridesExpectingError(ctx context.Context, cl client.Client, namespace string) error {
	env := &component.Env{Client: cl, Namespace: namespace}

	plan, _, err := overrideTestComponent{}.Plan(ctx, env, nil)
	if err != nil {
		return err
	}

	snapshot, err := loadOverrideEntries(ctx, cl, namespace)
	if err != nil {
		// An unusable document drops the workloads it could have targeted,
		// mirroring what the operator does.
		for i := range plan.Operations {
			if plan.Operations[i].Overridable {
				plan.Operations = append(plan.Operations[:i], plan.Operations[i+1:]...)

				break
			}
		}

		if _, execErr := env.Execute(ctx, plan); execErr != nil {
			return execErr
		}

		return err
	}

	if len(snapshot) > 0 {
		report := override.Apply(plan, snapshot, nil)
		if report.Failed() {
			return report.Err()
		}
	}

	exec, err := env.Execute(ctx, plan)
	if err != nil {
		return err
	}

	return exec.Err()
}

// loadOverrideEntries reads and validates the overrides ConfigMap the way the
// operator does.
func loadOverrideEntries(ctx context.Context, cl client.Client, namespace string) ([]override.SourcedEntry, error) {
	var configMap corev1.ConfigMap

	key := client.ObjectKey{Namespace: namespace, Name: override.ConfigMapName}
	if err := cl.Get(ctx, key, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	entries, err := override.Parse(configMap.Data)
	if err != nil {
		return nil, err
	}

	if err := override.Validate(entries); err != nil {
		return nil, err
	}

	return entries, nil
}

func writeOverrides(ctx context.Context, t *testing.T, cl client.Client, namespace string, data map[string]string) {
	t.Helper()

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: override.ConfigMapName},
		Data:       data,
	}

	var existing corev1.ConfigMap

	err := cl.Get(ctx, client.ObjectKeyFromObject(configMap), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if createErr := cl.Create(ctx, configMap); createErr != nil {
			t.Fatalf("create overrides: %v", createErr)
		}
	case err != nil:
		t.Fatalf("get overrides: %v", err)
	default:
		existing.Data = data
		if updateErr := cl.Update(ctx, &existing); updateErr != nil {
			t.Fatalf("update overrides: %v", updateErr)
		}
	}
}

func deleteOverrides(ctx context.Context, t *testing.T, cl client.Client, namespace string) {
	t.Helper()

	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: override.ConfigMapName}}
	if err := cl.Delete(ctx, configMap); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete overrides: %v", err)
	}
}

func getDaemonSet(ctx context.Context, t *testing.T, cl client.Client, namespace, name string) *appsv1.DaemonSet {
	t.Helper()

	var got appsv1.DaemonSet

	if err := utilwait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &got); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}

				return false, err
			}

			return true, nil
		}); err != nil {
		t.Fatalf("get DaemonSet %s/%s: %v", namespace, name, err)
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
