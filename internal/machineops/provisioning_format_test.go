// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type changingVersionClient struct {
	client.Client
	lists   int
	failure error
}

func (c *changingVersionClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	c.lists++
	if c.failure != nil {
		return c.failure
	}

	image, format := "ignition-image", api.ProvisioningFormatIgnition
	if c.lists > 1 {
		image, format = "cloud-init-image", api.ProvisioningFormatCloudInit
	}

	list.(*api.MachineConfigurationVersionList).Items = []api.MachineConfigurationVersion{{
		ObjectMeta: metav1.ObjectMeta{Name: image}, Spec: api.MachineConfigurationVersionSpec{
			Version: int32(c.lists), Template: api.MachineConfigurationTemplate{Host: &api.MachineConfigurationHostSpec{Image: image, ProvisioningFormat: format}},
		},
	}}

	return nil
}

func TestReplacementPairUsesOneVersionSelection(t *testing.T) {
	t.Parallel()

	for _, image := range []string{"", " ", "\t"} {
		for _, observed := range []api.ProvisioningFormat{"", api.ProvisioningFormatCloudInit} {
			c := &changingVersionClient{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).Build()}
			r := &MachineOperationReconciler{Client: c}
			m := newExternalMachine("worker", api.ExternalProviderAzureVM)
			m.Spec.Host = &api.HostSpec{Image: image}
			m.Spec.ConfigurationRef = &api.MachineConfigurationRef{Name: "worker"}
			m.Status.ObservedProvisioningFormat = observed
			input, err := r.resolveOperationTargetInput(t.Context(), newMachineOperation("replace", "worker", api.OperationHostReplace), m)
			require.NoError(t, err)
			require.Equal(t, 1, c.lists)
			require.Equal(t, "ignition-image", input.HostImage)
			require.Equal(t, api.ProvisioningFormatIgnition, input.ProvisioningFormat)
		}
	}
}

func TestReplacementFormatPrecedence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		host     *api.HostSpec
		observed api.ProvisioningFormat
		want     api.ProvisioningFormat
		wantErr  bool
	}{
		{name: "legacy", want: api.ProvisioningFormatCloudInit},
		{name: "observed ignition preserve image", observed: api.ProvisioningFormatIgnition, want: api.ProvisioningFormatIgnition},
		{name: "observed cloud-init", observed: api.ProvisioningFormatCloudInit, want: api.ProvisioningFormatCloudInit},
		{name: "explicit migration", host: &api.HostSpec{Image: "new", ProvisioningFormat: api.ProvisioningFormatCloudInit}, observed: api.ProvisioningFormatIgnition, want: api.ProvisioningFormatCloudInit},
		{name: "unknown target format", host: &api.HostSpec{Image: "new"}, observed: api.ProvisioningFormatIgnition, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := &api.Machine{Spec: api.MachineSpec{Host: tc.host}, Status: api.MachineStatus{ObservedProvisioningFormat: tc.observed}}

			image := ""
			if tc.host != nil {
				image = tc.host.Image
			}

			format, err := replacementProvisioningFormat(m, image)
			if tc.wantErr {
				require.ErrorContains(t, err, "explicit target")
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, format)
			}
		})
	}
}

func TestReplacementSnapshotAndImageOverride(t *testing.T) {
	t.Parallel()
	c := &changingVersionClient{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).Build()}
	r := &MachineOperationReconciler{Client: c}
	m := newExternalMachine("worker", api.ExternalProviderAzureVM)
	m.Spec.ConfigurationRef = &api.MachineConfigurationRef{Name: "worker"}
	op := newMachineOperation("replace", "worker", api.OperationHostReplace)
	input, err := r.resolveOperationTargetInput(t.Context(), op, m)
	require.NoError(t, err)

	m.Spec.Host = &api.HostSpec{Image: " other-image ", ProvisioningFormat: api.ProvisioningFormatCloudInit}
	_, err = r.operationRequest(t.Context(), op, m, &api.MachineOperationTargetStatus{Input: input}, m.Spec.ProviderID, nil, true)
	require.ErrorContains(t, err, "cannot generate Ignition")
	input, err = r.resolveOperationTargetInput(t.Context(), op, m)
	require.NoError(t, err)
	require.Equal(t, "other-image", input.HostImage)
	require.Equal(t, api.ProvisioningFormatCloudInit, input.ProvisioningFormat)
	require.Equal(t, 1, c.lists, "explicit image must not read template format")

	m.Spec.Host.ProvisioningFormat = ""
	m.Status.ObservedProvisioningFormat = api.ProvisioningFormatIgnition
	_, err = r.resolveOperationTargetInput(t.Context(), op, m)
	require.ErrorContains(t, err, "explicit target")
}

func TestReplacementVersionReadFailureRemainsRetryable(t *testing.T) {
	t.Parallel()

	injected := errors.New("API unavailable")
	c := &changingVersionClient{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).Build(), failure: injected}
	m := newExternalMachine("worker", api.ExternalProviderAzureVM)
	m.Spec.ConfigurationRef = &api.MachineConfigurationRef{Name: "worker"}
	_, err := (&MachineOperationReconciler{Client: c}).resolveReplacementHost(t.Context(), m)
	require.ErrorIs(t, err, injected)

	var permanent *targetInputError
	require.False(t, errors.As(err, &permanent))
}

func TestLegacyReplacementSnapshotHonorsKnownFormat(t *testing.T) {
	t.Parallel()

	r := &MachineOperationReconciler{}
	m := newExternalMachine("worker", api.ExternalProviderAzureVM)
	m.Status.ObservedProvisioningFormat = api.ProvisioningFormatIgnition

	op := newMachineOperation("replace", m.Name, api.OperationHostReplace)
	for _, input := range []*api.MachineOperationTargetInput{nil, {}} {
		_, err := r.operationRequest(t.Context(), op, m, &api.MachineOperationTargetStatus{Input: input}, m.Spec.ProviderID, nil, true)
		require.ErrorContains(t, err, "cannot generate Ignition")
	}
}

func TestFrozenCloudInitPairSurvivesDesiredEdit(t *testing.T) {
	t.Parallel()
	scheme := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: metav1.NamespaceSystem}, Data: map[string][]byte{"token-id": []byte("abc123"), "token-secret": []byte("secret456")}}
	r := &MachineOperationReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), ClusterInfo: testClusterInfo()}
	m := newExternalMachine("worker", api.ExternalProviderAzureVM)
	m.Spec.Host = &api.HostSpec{Image: "original-image", ProvisioningFormat: api.ProvisioningFormatCloudInit}
	m.Spec.Kubernetes = &api.KubernetesSpec{BootstrapTokenRef: &api.LocalObjectReference{Name: secret.Name}}
	op := newMachineOperation("replace", m.Name, api.OperationHostReplace)
	input, err := r.resolveOperationTargetInput(t.Context(), op, m)
	require.NoError(t, err)

	m.Spec.Host = &api.HostSpec{Image: "new-ignition-image", ProvisioningFormat: api.ProvisioningFormatIgnition}
	request, err := r.operationRequest(t.Context(), op, m, &api.MachineOperationTargetStatus{Input: input}, m.Spec.ProviderID, nil, true)
	require.NoError(t, err)
	require.Equal(t, "original-image", request.HostImage)
	require.Contains(t, request.ReplaceUserData, "#cloud-config")
}

func TestMachineFormatOverridesTemplateFormat(t *testing.T) {
	t.Parallel()
	c := &changingVersionClient{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).Build()}
	r := &MachineOperationReconciler{Client: c}
	m := newExternalMachine("worker", api.ExternalProviderAzureVM)
	m.Spec.ConfigurationRef = &api.MachineConfigurationRef{Name: "worker"}
	m.Spec.Host = &api.HostSpec{ProvisioningFormat: api.ProvisioningFormatCloudInit}
	host, err := r.resolveReplacementHost(t.Context(), m)
	require.NoError(t, err)
	require.Equal(t, "ignition-image", host.Image)
	require.Equal(t, api.ProvisioningFormatCloudInit, host.Format, "explicit target declaration overrides template")
}

func TestHostReplaceRefusesUnsupportedFormatBeforeProvider(t *testing.T) {
	t.Parallel()

	for _, declaration := range []string{"desired", "observed", "template"} {
		t.Run(declaration, func(t *testing.T) {
			t.Parallel()
			scheme := newOperationTestScheme(t)
			require.NoError(t, corev1.AddToScheme(scheme))

			m := newExternalMachine("machine-1", api.ExternalProviderAzureVM)
			m.Spec.Kubernetes = &api.KubernetesSpec{BootstrapTokenRef: &api.LocalObjectReference{Name: "bootstrap-token-test"}}
			version := int32(2)
			v := &api.MachineConfigurationVersion{
				ObjectMeta: metav1.ObjectMeta{Name: api.MachineConfigurationVersionName("worker", version)},
				Spec:       api.MachineConfigurationVersionSpec{Version: version, Template: api.MachineConfigurationTemplate{Host: &api.MachineConfigurationHostSpec{Image: "ignition", ProvisioningFormat: api.ProvisioningFormatIgnition}}},
			}

			switch declaration {
			case "desired":
				m.Spec.Host = &api.HostSpec{ProvisioningFormat: api.ProvisioningFormatIgnition}
			case "observed":
				m.Status.ObservedProvisioningFormat = api.ProvisioningFormatIgnition
			case "template":
				m.Spec.ConfigurationRef = &api.MachineConfigurationRef{Name: "worker", Version: &version}
			}

			op := newMachineOperation("op-1", m.Name, api.OperationHostReplace)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-test"}, Data: map[string][]byte{"token-id": []byte("abc123"), "token-secret": []byte("secret456")}}
			credential := newWorkloadIdentityCredential("cred-a", "site-a", api.ExternalProviderAzureVM)
			provider := &recordingProvider{provider: api.ExternalProviderAzureVM, supported: map[api.OperationKind]bool{api.OperationHostReplace: true}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m, op, secret, credential, v).WithStatusSubresource(op).Build()
			r := &MachineOperationReconciler{Client: c, Providers: []*Provider{newRecordingProviderRegistration(provider)}, Now: fixedOperationNow, ClusterInfo: testClusterInfo()}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: op.Name}})
			require.NoError(t, err)
			require.Empty(t, provider.calls)
			require.Empty(t, provider.replaceUserData)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			require.Equal(t, api.OperationPhaseFailed, op.Status.Phase)
			require.Contains(t, op.Status.Message, "cannot generate Ignition")
		})
	}
}
