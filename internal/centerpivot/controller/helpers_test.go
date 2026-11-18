// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/centerpivot/drain"
	"github.com/Azure/unbounded/internal/centerpivot/identity"
	"github.com/Azure/unbounded/internal/centerpivot/omaha"
)

const (
	running = "202606.15.0"
	newer   = "202606.29.0"

	aclApp    = "11111111-2222-3333-4444-555555555555"
	nvidiaApp = "22222222-2222-3333-4444-555555555555"
)

var start = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, unboundedv1alpha3.AddToScheme(scheme))

	return scheme
}

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithIndex(&unboundedv1alpha3.Machine{}, MachineNodeIndex, IndexMachineNode).
		Build()
}

// testSite returns a Site that enables centerpivot, allowing maxUnavailable
// nodes to update at once.
func testSite(name string, maxUnavailable *intstr.IntOrString) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			CenterPivot: &unboundedv1alpha3.CenterPivotComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)},
				NebraskaURL:       "https://nebraska.example.com/v1/update",
				Apps:              []unboundedv1alpha3.CenterPivotApp{{Name: "acl", AppID: aclApp, DefaultTrack: "stable"}},
				MaxUnavailable:    maxUnavailable,
			},
		}},
	}
}

// testNode returns a Ready managed ACL node in the Site, whose agent reports
// version.
func testNode(t *testing.T, name, site, version string) *corev1.Node {
	t.Helper()

	info, err := identity.NewAgentInfo("34a267d412544d6a9e97b5ba6762f6e7", version)
	require.NoError(t, err)

	value, err := info.Marshal()
	require.NoError(t, err)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Labels: map[string]string{
				ManagedLabel:                          "true",
				unboundedv1alpha3.MachineSiteLabelKey: site,
				corev1.LabelArchStable:                "amd64",
			},
			Annotations: map[string]string{AgentInfoAnnotation: value},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func testMachine(t *testing.T, name string, state *State) *unboundedv1alpha3.Machine {
	t.Helper()

	machine := &unboundedv1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}

	if state != nil {
		value, err := state.Marshal()
		require.NoError(t, err)

		machine.Annotations = map[string]string{StateAnnotation: value}
	}

	return machine
}

func machineOf(t *testing.T, c client.Client, name string) *unboundedv1alpha3.Machine {
	t.Helper()

	var machine unboundedv1alpha3.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, &machine))

	return &machine
}

func stateOf(t *testing.T, c client.Client, name string) State {
	t.Helper()

	return StateOf(machineOf(t, c, name))
}

// fakeNebraska answers every update check with result.
type fakeNebraska struct {
	result omaha.Result
	err    error
	checks []omaha.Check
}

func (f *fakeNebraska) UpdateCheck(_ context.Context, _ string, check omaha.Check) (omaha.Result, error) {
	f.checks = append(f.checks, check)

	return f.result, f.err
}

// fakeDrainer drains as told.
type fakeDrainer struct {
	blocked   error
	left      int
	workloads []drain.Workload
	recovered bool
}

func (f *fakeDrainer) Check(context.Context, string, drain.Options) error { return f.blocked }

func (f *fakeDrainer) Evict(context.Context, string, drain.Options) (int, error) { return f.left, nil }

func (f *fakeDrainer) Workloads(context.Context, string, drain.Options) ([]drain.Workload, error) {
	return f.workloads, nil
}

func (f *fakeDrainer) Recovered(context.Context, []drain.Workload) (bool, error) {
	return f.recovered, nil
}
