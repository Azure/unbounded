// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func newMCTestClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(
			&v1alpha3.MachineConfiguration{},
			&v1alpha3.MachineConfigurationVersion{},
		).
		Build()
}

func newMC(name string) *v1alpha3.MachineConfiguration {
	return &v1alpha3.MachineConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("mc-uid-" + name),
		},
		Spec: v1alpha3.MachineConfigurationSpec{
			Template: v1alpha3.MachineConfigurationTemplate{
				Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
					Version: "v1.34.0",
				},
				Agent: &v1alpha3.MachineConfigurationAgent{
					Image: "ghcr.io/azure/agent:v1",
				},
			},
		},
	}
}

func reconcileMC(t *testing.T, c client.Client, name string) {
	t.Helper()

	r := &MachineConfigurationReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)
}

func getMC(t *testing.T, c client.Client, name string) *v1alpha3.MachineConfiguration {
	t.Helper()

	var mc v1alpha3.MachineConfiguration
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &mc))

	return &mc
}

func getMCV(t *testing.T, c client.Client, name string) *v1alpha3.MachineConfigurationVersion {
	t.Helper()

	var mcv v1alpha3.MachineConfigurationVersion
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &mcv))

	return &mcv
}

func listMCVs(t *testing.T, c client.Client, mcName string) []v1alpha3.MachineConfigurationVersion {
	t.Helper()

	var list v1alpha3.MachineConfigurationVersionList
	require.NoError(t, c.List(context.Background(), &list, client.MatchingLabels{
		v1alpha3.MCVConfigurationLabelKey: mcName,
	}))

	return list.Items
}

func Test_MCReconcile_CreatesInitialVersion(t *testing.T) {
	mc := newMC("myconfig")
	c := newMCTestClient(mc)

	reconcileMC(t, c, "myconfig")

	// Should create myconfig-v1.
	mcv := getMCV(t, c, "myconfig-v1")
	assert.Equal(t, int32(1), mcv.Spec.Version)
	assert.Equal(t, "v1.34.0", mcv.Spec.Template.Kubernetes.Version)
	assert.Equal(t, "ghcr.io/azure/agent:v1", mcv.Spec.Template.Agent.Image)

	// Labels should be set.
	assert.Equal(t, "myconfig", mcv.Labels[v1alpha3.MCVConfigurationLabelKey])
	assert.Equal(t, "1", mcv.Labels[v1alpha3.MCVVersionLabelKey])

	// Owner reference should be set.
	require.Len(t, mcv.OwnerReferences, 1)
	assert.Equal(t, "myconfig", mcv.OwnerReferences[0].Name)
	assert.Equal(t, "MachineConfiguration", mcv.OwnerReferences[0].Kind)

	// Status should be updated.
	mc = getMC(t, c, "myconfig")
	assert.Equal(t, int32(1), mc.Status.LatestVersion)
	assert.Equal(t, int32(1), mc.Status.CurrentVersion)
}

func Test_MCReconcile_UpdatesEditableVersion(t *testing.T) {
	mc := newMC("myconfig")
	c := newMCTestClient(mc)

	// First reconcile creates v1.
	reconcileMC(t, c, "myconfig")

	// Change the spec.
	mc = getMC(t, c, "myconfig")
	mc.Spec.Template.Kubernetes.Version = "v1.35.0"
	require.NoError(t, c.Update(context.Background(), mc))

	// Second reconcile should update v1 in place (not deployed yet).
	reconcileMC(t, c, "myconfig")

	mcv := getMCV(t, c, "myconfig-v1")
	assert.Equal(t, "v1.35.0", mcv.Spec.Template.Kubernetes.Version)

	// Should still only have one version.
	versions := listMCVs(t, c, "myconfig")
	assert.Len(t, versions, 1)

	mc = getMC(t, c, "myconfig")
	assert.Equal(t, int32(1), mc.Status.LatestVersion)
	assert.Equal(t, int32(1), mc.Status.CurrentVersion)
}

func Test_MCReconcile_CreatesNewVersionWhenDeployed(t *testing.T) {
	mc := newMC("myconfig")
	c := newMCTestClient(mc)

	// Create v1.
	reconcileMC(t, c, "myconfig")

	// Mark v1 as deployed (simulating a machine using it).
	mcv := getMCV(t, c, "myconfig-v1")
	mcv.Status.Deployed = true
	mcv.Status.DeployedMachines = 1
	require.NoError(t, c.Status().Update(context.Background(), mcv))

	// Change the spec.
	mc = getMC(t, c, "myconfig")
	mc.Spec.Template.Kubernetes.Version = "v1.35.0"
	require.NoError(t, c.Update(context.Background(), mc))

	// Reconcile should create v2.
	reconcileMC(t, c, "myconfig")

	// v1 should still have the old version.
	mcv1 := getMCV(t, c, "myconfig-v1")
	assert.Equal(t, "v1.34.0", mcv1.Spec.Template.Kubernetes.Version)
	assert.True(t, mcv1.Status.Deployed)

	// v2 should have the new version.
	mcv2 := getMCV(t, c, "myconfig-v2")
	assert.Equal(t, int32(2), mcv2.Spec.Version)
	assert.Equal(t, "v1.35.0", mcv2.Spec.Template.Kubernetes.Version)
	assert.False(t, mcv2.Status.Deployed)

	mc = getMC(t, c, "myconfig")
	assert.Equal(t, int32(2), mc.Status.LatestVersion)
	assert.Equal(t, int32(2), mc.Status.CurrentVersion)
}

func Test_MCReconcile_NoopWhenSpecUnchanged(t *testing.T) {
	mc := newMC("myconfig")
	c := newMCTestClient(mc)

	// First reconcile.
	reconcileMC(t, c, "myconfig")

	// Reconcile again without changes.
	reconcileMC(t, c, "myconfig")

	// Should still only have one version.
	versions := listMCVs(t, c, "myconfig")
	assert.Len(t, versions, 1)
}

func Test_MCReconcile_MultipleEditsCycleCorrectly(t *testing.T) {
	mc := newMC("myconfig")
	c := newMCTestClient(mc)

	// Create v1, deploy it.
	reconcileMC(t, c, "myconfig")

	mcv := getMCV(t, c, "myconfig-v1")
	mcv.Status.Deployed = true
	mcv.Status.DeployedMachines = 2
	require.NoError(t, c.Status().Update(context.Background(), mcv))

	// Edit 1 -> creates v2.
	mc = getMC(t, c, "myconfig")
	mc.Spec.Template.Kubernetes.Version = "v1.35.0"
	require.NoError(t, c.Update(context.Background(), mc))
	reconcileMC(t, c, "myconfig")

	// Edit 2 while v2 is not yet deployed -> updates v2 in place.
	mc = getMC(t, c, "myconfig")
	mc.Spec.Template.Kubernetes.Version = "v1.36.0"
	require.NoError(t, c.Update(context.Background(), mc))
	reconcileMC(t, c, "myconfig")

	versions := listMCVs(t, c, "myconfig")
	assert.Len(t, versions, 2)

	mcv2 := getMCV(t, c, "myconfig-v2")
	assert.Equal(t, "v1.36.0", mcv2.Spec.Template.Kubernetes.Version)

	// Deploy v2, then edit again -> creates v3.
	mcv2.Status.Deployed = true
	mcv2.Status.DeployedMachines = 1
	require.NoError(t, c.Status().Update(context.Background(), mcv2))

	mc = getMC(t, c, "myconfig")
	mc.Spec.Template.Agent.Image = "ghcr.io/azure/agent:v2"
	require.NoError(t, c.Update(context.Background(), mc))
	reconcileMC(t, c, "myconfig")

	versions = listMCVs(t, c, "myconfig")
	assert.Len(t, versions, 3)

	mcv3 := getMCV(t, c, "myconfig-v3")
	assert.Equal(t, int32(3), mcv3.Spec.Version)
	assert.Equal(t, "ghcr.io/azure/agent:v2", mcv3.Spec.Template.Agent.Image)
	assert.Equal(t, "v1.36.0", mcv3.Spec.Template.Kubernetes.Version)
}

func Test_MCReconcile_DeletedMCIsNoOp(t *testing.T) {
	c := newMCTestClient() // No MC exists.

	r := &MachineConfigurationReconciler{Client: c, Scheme: scheme}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent"},
	})

	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}
