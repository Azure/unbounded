// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

// siteCacheClient stands in for metalman's manager cache, which only holds
// Machines for its own site (see commands.SiteSelector). Other objects and
// writes pass through to the shared client.
type siteCacheClient struct {
	client.Client
	site string
}

func (c siteCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}

	if machine, ok := obj.(*v1alpha3.Machine); ok && !c.cached(machine) {
		return apierrors.NewNotFound(v1alpha3.Resource("machines"), key.Name)
	}

	return nil
}

func (c siteCacheClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}

	machines, ok := list.(*v1alpha3.MachineList)
	if !ok {
		return nil
	}

	items := machines.Items[:0]
	for _, machine := range machines.Items {
		if c.cached(&machine) {
			items = append(items, machine)
		}
	}

	machines.Items = items

	return nil
}

func (c siteCacheClient) cached(machine *v1alpha3.Machine) bool {
	site, ok := machine.Labels[siteLabel]
	if c.site == "" {
		return !ok
	}

	return ok && site == c.site
}

// siteReconciler builds a reconciler for site that shares c with every other
// site, reading Machines through a site-filtered cache and live reads
// through c, as metalman does.
func siteReconciler(c client.Client, power PowerClientFactory, site string) *Reconciler {
	r := testReconciler(siteCacheClient{Client: c, site: site}, power, site)
	r.APIReader = c

	return r
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) {
	t.Helper()

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)
}

func getOperation(t *testing.T, c client.Client, name string) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, &op))

	return op
}

func pendingTarget(machineName string) v1alpha3.MachineOperationTargetStatus {
	return v1alpha3.MachineOperationTargetStatus{MachineRef: machineName, Phase: v1alpha3.OperationPhasePending, Message: "target snapshotted"}
}

func TestOtherSiteDoesNotActOnOperation(t *testing.T) {
	t.Parallel()

	rackA := map[string]string{siteLabel: "rack-a"}

	tests := []struct {
		name      string
		otherSite string
		spec      func(*v1alpha3.MachineOperation)
	}{
		{
			name:      "unsnapshotted machineRef",
			otherSite: "rack-b",
			spec:      func(op *v1alpha3.MachineOperation) { op.Spec.MachineRef = "machine-a" },
		},
		{
			name:      "snapshotted machineRef",
			otherSite: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineRef = "machine-a"
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{pendingTarget("machine-a")}
			},
		},
		{
			name:      "unsnapshotted selector",
			otherSite: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: rackA}
			},
		},
		{
			name:      "snapshotted selector",
			otherSite: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: rackA}
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{pendingTarget("machine-a")}
			},
		},
		{
			name:      "selector with single-value In expression",
			otherSite: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: siteLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"rack-a"},
				}}}
			},
		},
		{
			name:      "default metalman and site-scoped selector",
			otherSite: "",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: rackA}
			},
		},
		{
			name:      "default metalman and snapshotted machineRef",
			otherSite: "",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineRef = "machine-a"
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{pendingTarget("machine-a")}
			},
		},
		{
			name:      "operation with boot media installed by the owning site",
			otherSite: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.OperationKind = v1alpha3.OperationHostReplace
				op.Spec.MachineRef = "machine-a"
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				target := pendingTarget("machine-a")
				target.Phase = v1alpha3.OperationPhaseInProgress
				target.BootMedia = &v1alpha3.MachineOperationBootMediaStatus{Site: "rack-a"}
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{target}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := testScheme(t)
			machine := testBareMetalMachine("machine-a", "rack-a")
			op := testOperation("op-a", v1alpha3.OperationHostPowerOff)
			tt.spec(op)

			c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
			before := getOperation(t, c, op.Name)
			power := &recordingPowerClient{}

			reconcileOnce(t, siteReconciler(c, power, tt.otherSite), op.Name)

			after := getOperation(t, c, op.Name)
			require.Equal(t, before.ResourceVersion, after.ResourceVersion, "other site wrote the operation: %+v", after.Status)
			require.Empty(t, power.calls)
		})
	}
}

func TestOwningSiteActsWhenOtherSiteReconcilesFirst(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineB := testBareMetalMachine("machine-b", "rack-b")
	refOp := testOperation("op-ref", v1alpha3.OperationHostPowerOff)
	refOp.Spec.MachineRef = machineA.Name
	selectorOp := testOperation("op-selector", v1alpha3.OperationHostPowerOff)
	selectorOp.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-b"}}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machineA, machineB, refOp, selectorOp, testRedfishSecret()).
		WithStatusSubresource(refOp, selectorOp).
		Build()
	powerA := &recordingPowerClient{}
	powerB := &recordingPowerClient{}
	siteA := siteReconciler(c, powerA, "rack-a")
	siteB := siteReconciler(c, powerB, "rack-b")

	// Alternate the sites, letting the non-owner go first each round.
	for range 3 {
		reconcileOnce(t, siteB, refOp.Name)
		reconcileOnce(t, siteA, refOp.Name)
		reconcileOnce(t, siteA, selectorOp.Name)
		reconcileOnce(t, siteB, selectorOp.Name)
	}

	for _, name := range []string{refOp.Name, selectorOp.Name} {
		updated := getOperation(t, c, name)
		require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase, "%s: %s", name, updated.Status.Message)
		require.Len(t, updated.Status.Targets, 1)
		require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Targets[0].Phase)
	}

	require.Equal(t, []string{"machine-a:ForceOff"}, powerA.calls)
	require.Equal(t, []string{"machine-b:ForceOff"}, powerB.calls)
}

func TestEverySiteFailsOperationNoSiteCanOwn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		site    string
		spec    func(*v1alpha3.MachineOperation)
		reason  string
		message string
	}{
		{
			name: "selector naming no site on a site metalman",
			site: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}
			},
			reason:  reasonInvalidTargetScope,
			message: "machineSelector must include unbounded-cloud.io/site=rack-b",
		},
		{
			name: "selector naming no site on the default metalman",
			site: "",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}
			},
			reason:  reasonInvalidTargetScope,
			message: "selector-based host operations require a non-empty metalman site",
		},
		{
			name: "selector with multi-value In expression",
			site: "rack-a",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: siteLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"rack-a", "rack-b"},
				}}}
			},
			reason:  reasonInvalidTargetScope,
			message: "machineSelector must include unbounded-cloud.io/site=rack-a",
		},
		{
			name: "selector naming two sites",
			site: "rack-b",
			spec: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{siteLabel: "rack-a"},
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: siteLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"rack-b"},
					}},
				}
			},
			reason:  reasonInvalidTargetScope,
			message: "machineSelector names more than one unbounded-cloud.io/site: rack-a, rack-b",
		},
		{
			name:    "machineRef to a missing Machine",
			site:    "rack-b",
			spec:    func(op *v1alpha3.MachineOperation) { op.Spec.MachineRef = "missing" },
			reason:  reasonMachineNotFound,
			message: "Machine missing not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := testScheme(t)
			machine := testBareMetalMachine("machine-a", "rack-a")
			op := testOperation("op-a", v1alpha3.OperationHostPowerOff)
			tt.spec(op)

			c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
			power := &recordingPowerClient{}

			reconcileOnce(t, siteReconciler(c, power, tt.site), op.Name)

			updated := getOperation(t, c, op.Name)
			require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
			require.Equal(t, tt.message, updated.Status.Message)
			require.Equal(t, tt.reason, updated.Status.Conditions[0].Reason)
			require.Empty(t, power.calls)
		})
	}
}

func TestOwnsOperationReturnsMachineReadErrors(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-a", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineRef = "machine-a"
	readErr := errors.New("apiserver unavailable")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	r := testReconciler(c, &recordingPowerClient{}, "rack-a")
	r.APIReader = failingMachineReader{Reader: c, err: readErr}

	owned, err := r.ownsOperation(t.Context(), op)
	require.ErrorIs(t, err, readErr)
	require.False(t, owned)

	before := getOperation(t, c, op.Name)
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorIs(t, err, readErr)
	require.Equal(t, before.ResourceVersion, getOperation(t, c, op.Name).ResourceVersion)
}

type failingMachineReader struct {
	client.Reader
	err error
}

func (r failingMachineReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1alpha3.Machine); ok {
		return r.err
	}

	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestOwnsOperationUsesBootMediaSite(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	// The Machine moved to rack-b after rack-a installed boot media for it.
	machine := testBareMetalMachine("machine-a", "rack-b")
	op := testOperation("op-a", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
		BootMedia:  &v1alpha3.MachineOperationBootMediaStatus{Site: "rack-a"},
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	for site, want := range map[string]bool{"rack-a": true, "rack-b": false, "": false} {
		owned, err := siteReconciler(c, &recordingPowerClient{}, site).ownsOperation(t.Context(), op)
		require.NoError(t, err)
		require.Equal(t, want, owned, "site %q", site)
	}
}

func TestSelectorSites(t *testing.T) {
	t.Parallel()

	in := func(values ...string) metav1.LabelSelectorRequirement {
		return metav1.LabelSelectorRequirement{Key: siteLabel, Operator: metav1.LabelSelectorOpIn, Values: values}
	}

	tests := []struct {
		name     string
		selector metav1.LabelSelector
		want     []string
	}{
		{name: "empty", want: nil},
		{name: "other labels", selector: metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}, want: nil},
		{name: "matchLabels", selector: metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}, want: []string{"rack-a"}},
		{name: "empty matchLabels value", selector: metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: ""}}, want: nil},
		{name: "single-value In", selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{in("rack-a")}}, want: []string{"rack-a"}},
		{name: "multi-value In", selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{in("rack-a", "rack-b")}}, want: nil},
		{
			name: "NotIn",
			selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: siteLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"rack-a"},
			}}},
			want: nil,
		},
		{
			name: "matchLabels and In agree",
			selector: metav1.LabelSelector{
				MatchLabels:      map[string]string{siteLabel: "rack-a"},
				MatchExpressions: []metav1.LabelSelectorRequirement{in("rack-a")},
			},
			want: []string{"rack-a"},
		},
		{
			name: "matchLabels and In disagree",
			selector: metav1.LabelSelector{
				MatchLabels:      map[string]string{siteLabel: "rack-b"},
				MatchExpressions: []metav1.LabelSelectorRequirement{in("rack-a")},
			},
			want: []string{"rack-a", "rack-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, selectorSites(&tt.selector))
		})
	}
}

func TestReconcilerSetsObservedMachineGenerationOnCompletion(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-a", "rack-a")
	machine.Generation = 7
	refOp := testOperation("op-ref", v1alpha3.OperationHostPowerOff)
	refOp.Spec.MachineRef = machine.Name
	selectorOp := testOperation("op-selector", v1alpha3.OperationHostPowerOn)
	selectorOp.CreationTimestamp = metav1.NewTime(fixedNow().Add(1))
	selectorOp.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, refOp, selectorOp, testRedfishSecret()).
		WithStatusSubresource(refOp, selectorOp).
		Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOn}}
	r := siteReconciler(c, power, "rack-a")

	reconcileOnce(t, r, refOp.Name)

	updated := getOperation(t, c, refOp.Name)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase, updated.Status.Message)
	require.Zero(t, updated.Status.ObservedMachineGeneration, "set before completion")

	reconcileOnce(t, r, refOp.Name)

	updated = getOperation(t, c, refOp.Name)
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase, updated.Status.Message)
	require.Equal(t, machine.Generation, updated.Status.ObservedMachineGeneration)

	for range 3 {
		reconcileOnce(t, r, selectorOp.Name)
	}

	updated = getOperation(t, c, selectorOp.Name)
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase, updated.Status.Message)
	require.Equal(t, machine.Generation, updated.Status.Targets[0].ObservedGeneration)
	require.Zero(t, updated.Status.ObservedMachineGeneration)
}

func TestReconcilerDoesNotSetObservedMachineGenerationOnFailure(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-a", "rack-a")
	machine.Generation = 3
	op := testOperation("op-ref", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	power := &recordingPowerClient{methodErrors: map[string]error{"factory": errors.New("bmc unreachable")}}
	r := siteReconciler(c, power, "rack-a")
	r.MaxAttempts = 1

	for range 4 {
		reconcileOnce(t, r, op.Name)
	}

	updated := getOperation(t, c, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase, updated.Status.Message)
	require.Zero(t, updated.Status.ObservedMachineGeneration)
}
