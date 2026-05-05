// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func newConfigurationTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(
			&unboundedv1alpha3.Machine{},
			&unboundedv1alpha3.MachineConfiguration{},
			&unboundedv1alpha3.MachineConfigurationVersion{},
		).
		Build()
}

func newTestMachineConfiguration(name string) *unboundedv1alpha3.MachineConfiguration {
	return &unboundedv1alpha3.MachineConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: unboundedv1alpha3.MachineConfigurationSpec{
			Template: unboundedv1alpha3.MachineConfigurationTemplate{
				Kubernetes: &unboundedv1alpha3.MachineConfigurationKubernetes{
					Version: "v1.34.0",
				},
				Agent: &unboundedv1alpha3.MachineConfigurationAgent{
					Image: "ghcr.io/test/rootfs:v1",
				},
			},
		},
	}
}

func reconcileMachineConfiguration(t *testing.T, c client.Client, name string) {
	t.Helper()

	r := &MachineConfigurationReconciler{Client: c, Scheme: newTestScheme(t)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	require.NoError(t, err)
}

func getMachineConfiguration(t *testing.T, c client.Client, name string) *unboundedv1alpha3.MachineConfiguration {
	t.Helper()

	var mc unboundedv1alpha3.MachineConfiguration
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &mc))

	return &mc
}

func getMachineConfigurationVersion(
	t *testing.T,
	c client.Client,
	name string,
) *unboundedv1alpha3.MachineConfigurationVersion {
	t.Helper()

	var mcv unboundedv1alpha3.MachineConfigurationVersion
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &mcv))

	return &mcv
}

func TestMachineConfigurationReconciler_CreatesInitialVersion(t *testing.T) {
	t.Parallel()

	mc := newTestMachineConfiguration("config-a")
	c := newConfigurationTestClient(t, mc)

	reconcileMachineConfiguration(t, c, "config-a")

	mcv := getMachineConfigurationVersion(t, c, "config-a-v1")
	require.Equal(t, int32(1), mcv.Spec.Version)
	require.Equal(t, "v1.34.0", mcv.Spec.Template.Kubernetes.Version)
	require.Equal(t, "ghcr.io/test/rootfs:v1", mcv.Spec.Template.Agent.Image)
	require.Equal(t, "config-a", mcv.Labels[unboundedv1alpha3.MCVConfigurationLabelKey])
	require.Equal(t, "1", mcv.Labels[unboundedv1alpha3.MCVVersionLabelKey])
	require.Len(t, mcv.OwnerReferences, 1)

	mc = getMachineConfiguration(t, c, "config-a")
	require.Equal(t, int32(1), mc.Status.LatestVersion)
}

func TestMachineConfigurationReconciler_UpdatesUndeployedVersion(t *testing.T) {
	t.Parallel()

	mc := newTestMachineConfiguration("config-a")
	c := newConfigurationTestClient(t, mc)

	reconcileMachineConfiguration(t, c, "config-a")

	mc = getMachineConfiguration(t, c, "config-a")
	mc.Spec.Template.Kubernetes.Version = "v1.35.0"
	require.NoError(t, c.Update(context.Background(), mc))

	reconcileMachineConfiguration(t, c, "config-a")

	mcv := getMachineConfigurationVersion(t, c, "config-a-v1")
	require.Equal(t, "v1.35.0", mcv.Spec.Template.Kubernetes.Version)

	var list unboundedv1alpha3.MachineConfigurationVersionList
	require.NoError(t, c.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
}

func TestMachineConfigurationReconciler_CreatesNewVersionWhenLatestDeployed(t *testing.T) {
	t.Parallel()

	mc := newTestMachineConfiguration("config-a")
	c := newConfigurationTestClient(t, mc)

	reconcileMachineConfiguration(t, c, "config-a")

	mcv1 := getMachineConfigurationVersion(t, c, "config-a-v1")
	mcv1.Status.Deployed = true
	mcv1.Status.DeployedMachines = 1
	require.NoError(t, c.Status().Update(context.Background(), mcv1))

	mc = getMachineConfiguration(t, c, "config-a")
	mc.Spec.Template.Agent.Image = "ghcr.io/test/rootfs:v2"
	require.NoError(t, c.Update(context.Background(), mc))

	reconcileMachineConfiguration(t, c, "config-a")

	mcv1 = getMachineConfigurationVersion(t, c, "config-a-v1")
	require.Equal(t, "ghcr.io/test/rootfs:v1", mcv1.Spec.Template.Agent.Image)
	require.True(t, mcv1.Status.Deployed)

	mcv2 := getMachineConfigurationVersion(t, c, "config-a-v2")
	require.Equal(t, int32(2), mcv2.Spec.Version)
	require.Equal(t, "ghcr.io/test/rootfs:v2", mcv2.Spec.Template.Agent.Image)

	mc = getMachineConfiguration(t, c, "config-a")
	require.Equal(t, int32(2), mc.Status.LatestVersion)
}

func TestMachineConfigurationBindingReconciler_SetsPendingWhenNoMatch(t *testing.T) {
	t.Parallel()

	machine := newTestMachine("machine-a", "10.0.0.1:22", "testuser", defaultKubernetes())
	c := newConfigurationTestClient(t, machine)

	r := &MachineConfigurationBindingReconciler{Client: c, Scheme: newTestScheme(t)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "machine-a"}})
	require.NoError(t, err)

	var updated unboundedv1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "machine-a"}, &updated))
	require.Nil(t, updated.Spec.ConfigurationRef)

	cond := apimeta.FindStatusCondition(
		updated.Status.Conditions,
		unboundedv1alpha3.MachineConditionConfigurationPending,
	)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestMachineConfigurationBindingReconciler_PinsExplicitLatestVersion(t *testing.T) {
	t.Parallel()

	machine := newTestMachine("machine-a", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Spec.ConfigurationRef = &unboundedv1alpha3.MachineConfigurationRef{Name: "config-a"}
	mc := newTestMachineConfiguration("config-a")
	mcv := machineConfigurationVersion("config-a", 3)
	c := newConfigurationTestClient(t, machine, mc, mcv)

	r := &MachineConfigurationBindingReconciler{Client: c, Scheme: newTestScheme(t)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "machine-a"}})
	require.NoError(t, err)

	var updated unboundedv1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "machine-a"}, &updated))
	require.NotNil(t, updated.Spec.ConfigurationRef)
	require.Equal(t, "config-a", updated.Spec.ConfigurationRef.Name)
	require.NotNil(t, updated.Spec.ConfigurationRef.Version)
	require.Equal(t, int32(3), *updated.Spec.ConfigurationRef.Version)
	require.Equal(t, "v1.34.0", updated.Spec.Kubernetes.Version)
	require.Nil(t, updated.Spec.Agent)

	cond := apimeta.FindStatusCondition(
		updated.Status.Conditions,
		unboundedv1alpha3.MachineConditionConfigurationPending,
	)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestMachineConfigurationBindingReconciler_SelectsByPriorityThenName(t *testing.T) {
	t.Parallel()

	machine := newTestMachine("machine-a", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Labels = map[string]string{"role": "worker"}

	low := newTestMachineConfiguration("config-low")
	low.Spec.Priority = 1
	low.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}}

	highB := newTestMachineConfiguration("config-b")
	highB.Spec.Priority = 10
	highB.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}}

	highA := newTestMachineConfiguration("config-a")
	highA.Spec.Priority = 10
	highA.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}}

	c := newConfigurationTestClient(t,
		machine,
		low, highB, highA,
		machineConfigurationVersion("config-low", 1),
		machineConfigurationVersion("config-b", 1),
		machineConfigurationVersion("config-a", 2),
	)

	r := &MachineConfigurationBindingReconciler{Client: c, Scheme: newTestScheme(t)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "machine-a"}})
	require.NoError(t, err)

	var updated unboundedv1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "machine-a"}, &updated))
	require.NotNil(t, updated.Spec.ConfigurationRef)
	require.Equal(t, "config-a", updated.Spec.ConfigurationRef.Name)
	require.Equal(t, int32(2), *updated.Spec.ConfigurationRef.Version)
}

func machineConfigurationVersion(
	configurationName string,
	version int32,
) *unboundedv1alpha3.MachineConfigurationVersion {
	return &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: unboundedv1alpha3.MachineConfigurationVersionName(configurationName, version),
			Labels: map[string]string{
				unboundedv1alpha3.MCVConfigurationLabelKey: configurationName,
			},
		},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version: version,
			Template: unboundedv1alpha3.MachineConfigurationTemplate{
				Kubernetes: &unboundedv1alpha3.MachineConfigurationKubernetes{
					Version: "v1.99.0",
				},
				Agent: &unboundedv1alpha3.MachineConfigurationAgent{
					Image: "ghcr.io/test/rootfs:mcv",
				},
			},
		},
	}
}
