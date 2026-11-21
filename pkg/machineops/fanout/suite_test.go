// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

var (
	testEnvironment *envtest.Environment
	testRESTConfig  *rest.Config
	testScheme      *runtime.Scheme
	testNameIndex   atomic.Uint64
)

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "add client-go scheme: %v\n", err)
		os.Exit(1)
	}

	if err := unboundedv1alpha3.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "add unbounded scheme: %v\n", err)
		os.Exit(1)
	}

	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		os.Exit(m.Run())
	}

	testEnvironment = &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := testEnvironment.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}

	testRESTConfig = restConfig

	code := m.Run()

	if err := testEnvironment.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)

		code = 1
	}

	os.Exit(code)
}

type harness struct {
	t          *testing.T
	ctx        context.Context
	client     client.Client
	reconciler *reconciler
	now        time.Time
	prefix     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	c := newTestClient(t)

	h := &harness{
		t:      t,
		ctx:    t.Context(),
		client: c,
		now:    time.Now().UTC(),
		prefix: testName(t),
	}

	h.reconciler = &reconciler{
		Reader:               c,
		Writer:               c,
		APIReader:            c,
		PickupTimeout:        5 * time.Minute,
		BatchSize:            defaultBatchSize,
		MinReconcileInterval: defaultMinReconcileInterval,
		Now:                  func() time.Time { return h.now },
	}

	return h
}

// newTestClient returns a client for a real API server when envtest assets
// are present, and an in-memory one otherwise. The controller's own logic is
// the same against both, so the suite also runs where the assets are not
// installed. Only the real API server validates objects against the CRD and
// garbage-collects children.
func newTestClient(t *testing.T) client.Client {
	t.Helper()

	if testRESTConfig != nil {
		c, err := client.New(testRESTConfig, client.Options{Scheme: testScheme})
		require.NoError(t, err)

		return c
	}

	return fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&unboundedv1alpha3.MachineOperation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// The API server assigns a UID, a generation and a creation
			// time; the controller reads all three.
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
				if object.GetUID() == "" {
					object.SetUID(types.UID(fmt.Sprintf("uid-%d", testNameIndex.Add(1))))
				}

				if object.GetGeneration() == 0 {
					object.SetGeneration(1)
				}

				if created := object.GetCreationTimestamp(); created.IsZero() {
					object.SetCreationTimestamp(metav1.NewTime(time.Now().UTC().Truncate(time.Second)))
				}

				return c.Create(ctx, object, options...)
			},
		}).
		Build()
}

// requireAPIServer skips a test of something only a real API server does.
func requireAPIServer(t *testing.T) {
	t.Helper()

	if testRESTConfig == nil {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}
}

func testName(t *testing.T) string {
	t.Helper()

	value := strings.ToLower(t.Name())
	value = strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(value)

	value = strings.Trim(value, "-")
	if len(value) > 100 {
		value = value[:100]
	}

	return fmt.Sprintf("%s-%d", value, testNameIndex.Add(1))
}

func intOrString(value intstr.IntOrString) *intstr.IntOrString {
	return &value
}

func (h *harness) createMachine(name string, labels map[string]string) *unboundedv1alpha3.Machine {
	h.t.Helper()

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}

	require.NoError(h.t, h.client.Create(h.ctx, machine))

	return machine
}

func (h *harness) createDeletingMachine(name string, labels map[string]string) *unboundedv1alpha3.Machine {
	h.t.Helper()

	machine := h.createMachine(name, labels)
	machine.Finalizers = []string{"fanout.test/block-deletion"}
	require.NoError(h.t, h.client.Update(h.ctx, machine))
	require.NoError(h.t, h.client.Delete(h.ctx, machine))

	var current unboundedv1alpha3.Machine
	require.NoError(h.t, h.client.Get(h.ctx, client.ObjectKey{Name: name}, &current))
	require.False(h.t, current.DeletionTimestamp.IsZero())

	return &current
}

func (h *harness) createParent(
	name string,
	selector *metav1.LabelSelector,
	rollout *unboundedv1alpha3.MachineOperationRollout,
	ttl *int32,
	parameters map[string]string,
) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	parent := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineSelector:         selector,
			Rollout:                 rollout,
			OperationKind:           unboundedv1alpha3.OperationNodeReboot,
			Parameters:              parameters,
			TTLSecondsAfterFinished: ttl,
		},
	}

	require.NoError(h.t, h.client.Create(h.ctx, parent))

	return parent
}

func (h *harness) reconcile(name string) ctrl.Result {
	h.t.Helper()

	result, err := h.reconciler.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(h.t, err)

	return result
}

func (h *harness) getParent(name string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	var parent unboundedv1alpha3.MachineOperation
	require.NoError(h.t, h.client.Get(h.ctx, client.ObjectKey{Name: name}, &parent))

	return &parent
}

func (h *harness) getChild(name string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	var child unboundedv1alpha3.MachineOperation
	require.NoError(h.t, h.client.Get(h.ctx, client.ObjectKey{Name: name}, &child))

	return &child
}

func (h *harness) childExists(name string) bool {
	h.t.Helper()

	var child unboundedv1alpha3.MachineOperation

	return h.client.Get(h.ctx, client.ObjectKey{Name: name}, &child) == nil
}

func (h *harness) childForMachine(parent *unboundedv1alpha3.MachineOperation, machine string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	return h.getChild(childName(parent, machine))
}

func (h *harness) children(parent *unboundedv1alpha3.MachineOperation) []unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	var list unboundedv1alpha3.MachineOperationList
	require.NoError(h.t, h.client.List(h.ctx, &list, client.MatchingLabels{
		unboundedv1alpha3.MachineOperationParentUIDLabel: string(parent.UID),
	}))

	children := make([]unboundedv1alpha3.MachineOperation, 0, len(list.Items))
	for i := range list.Items {
		child := list.Items[i]
		if metav1.IsControlledBy(&child, parent) {
			children = append(children, child)
		}
	}

	sort.Slice(children, func(i, j int) bool {
		return children[i].Name < children[j].Name
	})

	return children
}

func (h *harness) childNames(parent *unboundedv1alpha3.MachineOperation) []string {
	h.t.Helper()

	children := h.children(parent)

	names := make([]string, 0, len(children))
	for i := range children {
		names = append(names, children[i].Name)
	}

	return names
}

func (h *harness) updateChild(
	name string,
	update func(child *unboundedv1alpha3.MachineOperation),
) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	child := h.getChild(name)
	update(child)

	require.NoError(h.t, h.client.Status().Update(h.ctx, child))

	return h.getChild(name)
}

func (h *harness) runChild(name, message string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	now := metav1.NewTime(h.now)

	return h.updateChild(name, func(child *unboundedv1alpha3.MachineOperation) {
		child.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
		child.Status.Message = message
		child.Status.StartedAt = &now
	})
}

func (h *harness) completeChild(name string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	now := metav1.NewTime(h.now)

	return h.updateChild(name, func(child *unboundedv1alpha3.MachineOperation) {
		child.Status.Phase = unboundedv1alpha3.OperationPhaseComplete

		child.Status.Message = "done"
		if child.Status.StartedAt == nil {
			child.Status.StartedAt = &now
		}

		child.Status.CompletedAt = &now
	})
}

func (h *harness) failChild(name, message string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	now := metav1.NewTime(h.now)

	return h.updateChild(name, func(child *unboundedv1alpha3.MachineOperation) {
		child.Status.Phase = unboundedv1alpha3.OperationPhaseFailed

		child.Status.Message = message
		if child.Status.StartedAt == nil {
			child.Status.StartedAt = &now
		}

		child.Status.CompletedAt = &now
	})
}

func requireCompletedCondition(
	t *testing.T,
	operation *unboundedv1alpha3.MachineOperation,
	status metav1.ConditionStatus,
	reason string,
) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(
		operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionCompleted,
	)
	require.NotNil(t, condition)
	require.Equal(t, status, condition.Status)
	require.Equal(t, reason, condition.Reason)

	return condition
}

func requirePhase(t *testing.T, operation *unboundedv1alpha3.MachineOperation, phase unboundedv1alpha3.OperationPhase) {
	t.Helper()

	require.Equal(t, phase, operation.Status.Phase)
	require.Equal(t, phase == unboundedv1alpha3.OperationPhaseComplete ||
		phase == unboundedv1alpha3.OperationPhaseFailed, operation.Status.IsTerminal())
}

func requireTargetsResolved(t *testing.T, operation *unboundedv1alpha3.MachineOperation) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(
		operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionTargetsResolved,
	)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionTrue, condition.Status)
	require.Equal(t, "Resolved", condition.Reason)

	return condition
}

func requireNotResolved(t *testing.T, operation *unboundedv1alpha3.MachineOperation) {
	t.Helper()

	condition := apimeta.FindStatusCondition(
		operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionTargetsResolved,
	)
	if condition != nil {
		require.NotEqual(t, metav1.ConditionTrue, condition.Status)
	}

	require.Nil(t, operation.Status.Summary)
}

func requireSummary(
	t *testing.T,
	operation *unboundedv1alpha3.MachineOperation,
	total, held, active, complete, failed int,
) {
	t.Helper()

	require.NotNil(t, operation.Status.Summary)
	require.Equal(t, int32(total), operation.Status.Summary.Total)
	require.Equal(t, int32(held), operation.Status.Summary.Held)
	require.Equal(t, int32(active), operation.Status.Summary.Active)
	require.Equal(t, int32(complete), operation.Status.Summary.Complete)
	require.Equal(t, int32(failed), operation.Status.Summary.Failed)
	require.Equal(t,
		fmt.Sprintf("machines: %d total, %d held, %d active, %d complete, %d failed",
			total, held, active, complete, failed),
		operation.Status.Message)
	require.Empty(t, operation.Status.Targets)
}

func releaseCount(children []unboundedv1alpha3.MachineOperation) int {
	released := 0

	for i := range children {
		if !children[i].IsHeld() && !children[i].Status.IsTerminal() {
			released++
		}
	}

	return released
}
