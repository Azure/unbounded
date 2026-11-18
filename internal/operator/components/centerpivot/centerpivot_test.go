// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package centerpivot

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	cp "github.com/Azure/unbounded/internal/centerpivot"
	"github.com/Azure/unbounded/internal/centerpivot/controller"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	server = "https://nebraska.example.com/v1/update"
	appID  = "11111111-2222-3333-4444-555555555555"
)

func site(name string, enabled bool) unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			CenterPivot: &unboundedv1alpha3.CenterPivotComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(enabled)},
				NebraskaURL:       server,
				Apps:              []unboundedv1alpha3.CenterPivotApp{{Name: "acl", AppID: appID}},
			},
		}},
	}
}

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(scheme))

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
		Config:    component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1.2.3"},
	}
}

func TestEnabledFor(t *testing.T) {
	t.Parallel()

	assert.False(t, EnabledFor(&unboundedv1alpha3.Site{}), "off unless enabled")
	assert.True(t, EnabledFor(ptr.To(site("edge", true))))
	assert.False(t, EnabledFor(ptr.To(site("edge", false))))
}

func TestPlanInstallsTheServiceAndNodeInfo(t *testing.T) {
	t.Parallel()

	plan, result, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{site("edge", true), site("core", false)})
	require.NoError(t, err)
	assert.True(t, result.Ready, result.Message)

	var refs []string

	for _, op := range plan.Operations {
		refs = append(refs, op.Ref().String())

		switch op.Object.GetKind() {
		case "Deployment", "DaemonSet":
			assert.True(t, op.Overridable, op.Ref().String())

			containers, _, _ := unstructured.NestedSlice(op.Object.Object, "spec", "template", "spec", "containers")
			assert.Equal(t, "ghcr.io/azure/centerpivot:v1.2.3", containers[0].(map[string]any)["image"], op.Ref().String()) //nolint:errcheck
		}
	}

	for _, want := range []string{
		"Deployment/unbounded-system/centerpivot",
		"DaemonSet/unbounded-system/centerpivot-node-info",
		"ClusterRole/centerpivot",
		"ClusterRole/centerpivot-node-info",
		"ValidatingAdmissionPolicy/centerpivot-node-restriction",
		"ValidatingAdmissionPolicy/centerpivot-machine-restriction",
		"ValidatingAdmissionPolicy/centerpivot-node-info-restriction",
	} {
		assert.Contains(t, refs, want)
	}
}

func TestPlanRemovesEverythingWhenNoSiteEnablesIt(t *testing.T) {
	t.Parallel()

	plan, result, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{site("edge", false), {}})
	require.NoError(t, err)
	assert.Equal(t, component.ReasonDisabled, result.Reason)

	installed, _, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{site("edge", true)})
	require.NoError(t, err)

	var applied, deleted []string
	for _, op := range installed.Operations {
		applied = append(applied, op.Ref().String())
	}

	for _, op := range plan.Operations {
		assert.Equal(t, component.OpDelete, op.Kind)

		deleted = append(deleted, op.Ref().String())
	}

	slices.Sort(applied)
	slices.Sort(deleted)
	assert.Equal(t, applied, deleted, "cleanup deletes exactly what install applies")
}

func TestPlanReportsAHeldNode(t *testing.T) {
	t.Parallel()

	held, err := controller.State{Phase: controller.PhaseHeld, Reason: "the node didn't boot 202606.29.0"}.Marshal()
	require.NoError(t, err)

	machine := &unboundedv1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "acl-1", Annotations: map[string]string{cp.StateAnnotation: held},
	}}

	_, result, err := (Component{}).Plan(t.Context(), testEnv(t, machine), []unboundedv1alpha3.Site{site("edge", true)})
	require.NoError(t, err)
	assert.False(t, result.Ready)
	assert.Equal(t, ReasonUpdatesHeld, result.Reason)
	assert.Contains(t, result.Message, "acl-1")
	assert.Contains(t, result.Message, "didn't boot")
}
