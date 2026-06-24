// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestMachineOperationReconciler_CompletesSupportedOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Generation = 3
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newWorkloadIdentityCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ignored", Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, updated.Status.Phase)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
	require.Equal(t, machine.Generation, updated.Status.ObservedMachineGeneration)
	require.Equal(t, []string{"HostReboot:machine-1:azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/machine-1"}, provider.calls)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestMachineOperationReconciler_SkipsOperationForDifferentSite(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{
		Client:       c,
		Providers:    []Provider{provider},
		SiteName:     "site-b",
		ProviderName: unboundedv1alpha3.ExternalProviderAzureVM,
		Now:          fixedOperationNow,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
	require.Empty(t, updated.Status.Conditions)
}

func TestMachineOperationReconciler_SkipsOperationForDifferentProviderScope(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderOCIInstance)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{
		Client:       c,
		Providers:    []Provider{provider},
		SiteName:     "site-a",
		ProviderName: unboundedv1alpha3.ExternalProviderAzureVM,
		Now:          fixedOperationNow,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
	require.Empty(t, updated.Status.Conditions)
}

func TestMachineOperationReconciler_ResolvesSiteCredential(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := &unboundedv1alpha3.MachineOperationCredential{
		ObjectMeta: metav1ObjectMeta("site-a-azure"),
		Spec: unboundedv1alpha3.MachineOperationCredentialSpec{
			SiteName: "site-a",
			Provider: unboundedv1alpha3.ExternalProviderAzureVM,
			Auth: unboundedv1alpha3.MachineOperationCredentialAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
				SecretRef: &unboundedv1alpha3.NamespacedSecretReference{
					Namespace: "unbounded-kube",
					Name:      "site-a-azure-sp",
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-kube", Name: "site-a-azure-sp"},
		Data: map[string][]byte{
			"tenantID":     []byte("tenant"),
			"clientID":     []byte("client"),
			"clientSecret": []byte("secret"),
		},
	}
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential, secret).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, []unboundedv1alpha3.MachineOperationCredentialAuthMode{unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin}, provider.authModes)
	require.Equal(t, "tenant", provider.authData[0]["tenantID"])
	require.Equal(t, "client", provider.authData[0]["clientID"])
	require.Equal(t, "secret", provider.authData[0]["clientSecret"])
}

func TestOperationAuthTargetForUsesNetSiteLabel(t *testing.T) {
	t.Parallel()

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Labels = map[string]string{netv1alpha1.SiteLabelKey: "site-a"}

	target, failure := operationAuthTargetFor(machine)

	require.Nil(t, failure)
	require.Equal(t, "site-a", target.SiteName)
	require.Equal(t, unboundedv1alpha3.ExternalProviderAzureVM, target.Provider)
}

func TestOperationAuthTargetForAcceptsMatchingSiteLabels(t *testing.T) {
	t.Parallel()

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Labels = map[string]string{
		unboundedv1alpha3.MachineSiteLabelKey: "site-a",
		netv1alpha1.SiteLabelKey:              "site-a",
	}

	target, failure := operationAuthTargetFor(machine)

	require.Nil(t, failure)
	require.Equal(t, "site-a", target.SiteName)
}

func TestOperationAuthTargetForRejectsConflictingSiteLabels(t *testing.T) {
	t.Parallel()

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Labels = map[string]string{
		unboundedv1alpha3.MachineSiteLabelKey: "site-a",
		netv1alpha1.SiteLabelKey:              "site-b",
	}

	_, failure := operationAuthTargetFor(machine)

	require.NotNil(t, failure)
	require.Equal(t, authReasonInvalid, failure.Reason)
	require.Contains(t, failure.Message, "conflicting site labels")
}

func TestMachineOperationReconciler_FailsMachineWithoutSiteLabel(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Labels = nil
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonInvalid, cond.Reason)
	require.Contains(t, updated.Status.Message, "missing site label")
}

func TestMachineOperationReconciler_FailsMissingSiteCredential(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonNotFound, cond.Reason)
}

func TestMachineOperationReconciler_PassesWorkloadIdentityCredentialToProvider(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newWorkloadIdentityCredential("site-a-azure", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, []unboundedv1alpha3.MachineOperationCredentialAuthMode{unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity}, provider.authModes)
	require.Nil(t, provider.authData[0])
}

func TestMachineOperationReconciler_FailsAmbiguousSiteCredential(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credentialA := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	credentialB := newMachineOperationCredential("cred-b", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credentialA, credentialB).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "multiple MachineOperationCredentials")

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonAmbiguous, cond.Reason)
}

func TestMachineOperationReconciler_FailsMissingCredentialSecret(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonSecretNotFound, cond.Reason)
}

func TestMachineOperationReconciler_FailsExternalPluginWithoutSecretRef(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	credential.Spec.Auth.SecretRef = nil
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonInvalid, cond.Reason)
}

func TestMachineOperationReconciler_FailsCredentialWithEmptyAuthMode(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	credential.Spec.Auth.Mode = ""
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonInvalid, cond.Reason)
}

func TestMachineOperationReconciler_FailsCredentialSecretOutsideAllowedNamespace(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	credential := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	credential.Spec.Auth.SecretRef.Namespace = "other"
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{
		Client:                    c,
		Providers:                 []Provider{provider},
		Now:                       fixedOperationNow,
		CredentialSecretNamespace: "unbounded-kube",
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, authReasonSecretForbidden, cond.Reason)
}

func TestOperationAuthRequiredSecretValuePreservesOpaqueValue(t *testing.T) {
	t.Parallel()

	auth := &OperationAuth{SecretData: map[string]string{"clientSecret": " secret-with-padding\n"}}

	value, err := auth.RequiredSecretValue("clientSecret")

	require.NoError(t, err)
	require.Equal(t, " secret-with-padding\n", value)
}

func TestMachineOperationReconciler_BuildsReplaceUserData(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{BootstrapTokenRef: &unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-test"}}
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReplace)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-test"},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("secret456"),
		},
	}
	credential := newWorkloadIdentityCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReplace: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, secret, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{
		Client:      c,
		Providers:   []Provider{provider},
		Now:         fixedOperationNow,
		ClusterInfo: testClusterInfo(),
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Len(t, provider.replaceUserData, 1)
	require.Contains(t, provider.replaceUserData[0], "#cloud-config")
	require.Contains(t, provider.replaceUserData[0], "abc123.secret456")
	require.Contains(t, provider.replaceUserData[0], `"NodeName": "machine-1"`)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, updated.Status.Phase)
}

func TestMachineOperationReconciler_DoesNotReexecuteInProgressOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.StartedAt = ptrTo(fixedOperationNow())
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletedAt)
}

func TestMachineOperationReconciler_DoesNotResolveAuthForSkippedInProgressOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.StartedAt = ptrTo(fixedOperationNow())
	credentialA := newMachineOperationCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	credentialB := newMachineOperationCredential("cred-b", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, credentialA, credentialB).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletedAt)
}

func TestMachineOperationReconciler_DoesNotReexecuteInProgressHostReplace(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{BootstrapTokenRef: &unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-test"}}
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReplace)
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.StartedAt = ptrTo(fixedOperationNow())
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-test"},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("secret456"),
		},
	}
	credential := newWorkloadIdentityCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderAzureVM)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReplace: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, secret, credential).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow, ClusterInfo: testClusterInfo()}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)
	require.Empty(t, provider.replaceUserData)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletedAt)
}

func TestMachineOperationReconciler_PatchesReplacementProviderIDBeforeCleanup(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))

	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderOCIInstance)
	machine.Spec.ProviderID = "oci://old-instance"
	machine.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{BootstrapTokenRef: &unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-test"}}
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReplace)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-test"},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("secret456"),
		},
	}
	credential := newWorkloadIdentityCredential("cred-a", "site-a", unboundedv1alpha3.ExternalProviderOCIInstance)
	provider := &recordingProvider{
		provider:  unboundedv1alpha3.ExternalProviderOCIInstance,
		supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReplace: true},
		result:    OperationResult{ProviderID: "oci://new-instance", CleanupProviderID: "oci://old-instance"},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, secret, credential).WithStatusSubresource(op).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}

			if updated, ok := obj.(*unboundedv1alpha3.Machine); ok {
				updated.Generation = 2
				return c.Update(ctx, updated)
			}

			return nil
		},
	}).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow, ClusterInfo: testClusterInfo()}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updatedMachine unboundedv1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "machine-1"}, &updatedMachine))
	require.Equal(t, "oci://new-instance", updatedMachine.Spec.ProviderID)
	require.Equal(t, []string{"HostReplace:machine-1:oci://old-instance"}, provider.calls)
	require.Equal(t, []string{"HostReplace:machine-1:oci://old-instance"}, provider.cleanupCalls)

	var updatedOp unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updatedOp))
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, updatedOp.Status.Phase)
	require.Equal(t, int64(2), updatedOp.Status.ObservedMachineGeneration)
}

func TestMachineOperationReconciler_UnsupportedOperationIsIgnored(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationNodeReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
}

func TestMachineOperationReconciler_FailsUnsupportedExternalOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderOCIInstance)
	machine.Spec.ProviderID = "oci://ocid1.instance.oc1.test"
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReplace)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderOCIInstance, supported: map[unboundedv1alpha3.OperationKind]bool{}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "HostReplace is not supported for OCIInstance")
}

func TestMachineOperationReconciler_SelectorOperationIsIgnored(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	op := newMachineOperation("op-1", "", unboundedv1alpha3.OperationHostReboot)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
}

func TestMachineOperationReconciler_FailsMissingMachine(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	op := newMachineOperation("op-1", "missing", unboundedv1alpha3.OperationHostReboot)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{&recordingProvider{}}, Now: fixedOperationNow}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "not found")
}

func TestMachineOperationReconciler_DeletesExpiredTerminalOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	completedAt := metav1.NewTime(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC))
	ttl := int32(30)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = unboundedv1alpha3.OperationPhaseComplete
	op.Status.CompletedAt = &completedAt

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Now: func() metav1.Time {
		return metav1.NewTime(completedAt.Add(31 * time.Second))
	}}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated unboundedv1alpha3.MachineOperation

	err = c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated)
	require.Error(t, err)
}

func TestShouldReconcileOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.Now()
	ttl := int32(30)

	tests := []struct {
		name string
		op   *unboundedv1alpha3.MachineOperation
		want bool
	}{
		{
			name: "non terminal",
			op:   newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot),
			want: true,
		},
		{
			name: "terminal without ttl",
			op: &unboundedv1alpha3.MachineOperation{
				Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
		{
			name: "terminal with ttl and completion time",
			op: &unboundedv1alpha3.MachineOperation{
				Spec: unboundedv1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: unboundedv1alpha3.MachineOperationStatus{
					Phase:       unboundedv1alpha3.OperationPhaseComplete,
					CompletedAt: &completedAt,
				},
			},
			want: true,
		},
		{
			name: "terminal with ttl but no completion time",
			op: &unboundedv1alpha3.MachineOperation{
				Spec:   unboundedv1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, shouldReconcileOperation(tt.op))
		})
	}
}

type recordingProvider struct {
	provider        string
	supported       map[unboundedv1alpha3.OperationKind]bool
	calls           []string
	cleanupCalls    []string
	replaceUserData []string
	authModes       []unboundedv1alpha3.MachineOperationCredentialAuthMode
	authData        []map[string]string
	result          OperationResult
	err             error
	cleanupErr      error
}

func (p *recordingProvider) Name() string {
	return p.provider
}

func (p *recordingProvider) Supports(operation unboundedv1alpha3.OperationKind) bool {
	return p.supported[operation]
}

func (p *recordingProvider) Execute(_ context.Context, request OperationRequest) (OperationResult, error) {
	p.calls = append(p.calls, fmt.Sprintf("%s:%s:%s", request.Operation, request.Machine.Name, request.ProviderID))
	if request.ReplaceUserData != "" {
		p.replaceUserData = append(p.replaceUserData, request.ReplaceUserData)
	}

	if request.Auth != nil {
		p.authModes = append(p.authModes, request.Auth.Mode)
		p.authData = append(p.authData, request.Auth.SecretData)
	}

	return p.result, p.err
}

func (p *recordingProvider) Cleanup(_ context.Context, request OperationRequest, result OperationResult) error {
	p.cleanupCalls = append(p.cleanupCalls, fmt.Sprintf("%s:%s:%s", request.Operation, request.Machine.Name, result.CleanupProviderID))
	return p.cleanupErr
}

func newOperationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))

	return s
}

func newMachineOperation(name, machineRef string, operation unboundedv1alpha3.OperationKind) *unboundedv1alpha3.MachineOperation {
	return &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			OperationKind: operation,
		},
	}
}

func newExternalMachine(name, provider string) *unboundedv1alpha3.Machine {
	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-a"},
		},
		Spec: unboundedv1alpha3.MachineSpec{
			Provider:   provider,
			ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/" + name,
		},
	}
}

func newWorkloadIdentityCredential(name, site, provider string) *unboundedv1alpha3.MachineOperationCredential {
	return &unboundedv1alpha3.MachineOperationCredential{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: unboundedv1alpha3.MachineOperationCredentialSpec{
			SiteName: site,
			Provider: provider,
			Auth: unboundedv1alpha3.MachineOperationCredentialAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity,
			},
		},
	}
}

func newMachineOperationCredential(name, site, provider string) *unboundedv1alpha3.MachineOperationCredential {
	return &unboundedv1alpha3.MachineOperationCredential{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: unboundedv1alpha3.MachineOperationCredentialSpec{
			SiteName: site,
			Provider: provider,
			Auth: unboundedv1alpha3.MachineOperationCredentialAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
				SecretRef: &unboundedv1alpha3.NamespacedSecretReference{
					Namespace: "unbounded-kube",
					Name:      name,
				},
			},
		},
	}
}

func fixedOperationNow() metav1.Time {
	return metav1.NewTime(time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
}

func ptrTo[T any](value T) *T {
	return &value
}

func testClusterInfo() *ClusterInfo {
	return &ClusterInfo{
		APIServer:    "api.example.com:443",
		CACertBase64: "Y2E=",
		ClusterDNS:   "10.0.0.10",
		KubeVersion:  "v1.34.0",
	}
}

func metav1ObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name}
}
