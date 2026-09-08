// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/provision"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func fakeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithObjects(objs...).
		Build()
}

// ---------------------------------------------------------------------------
// buildMachineCR
// ---------------------------------------------------------------------------

func Test_buildMachineCR(t *testing.T) {
	cfg := baseConfig()
	machine := buildMachineCR(cfg)

	assert.Equal(t, "test-machine", machine.Name)
	require.NotNil(t, machine.Spec.Kubernetes)
	require.NotNil(t, machine.Spec.Kubernetes.BootstrapTokenRef)
	assert.Equal(t, "bootstrap-token-abc123", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	assert.Equal(t, map[string]string{"env": "test"}, machine.Spec.Kubernetes.NodeLabels)
	assert.Equal(t, []string{"dedicated=test:NoSchedule"}, machine.Spec.Kubernetes.RegisterWithTaints)
}

func Test_buildMachineCR_TokenNoDot(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Auth.BootstrapToken = "abc123"
	machine := buildMachineCR(cfg)

	require.NotNil(t, machine.Spec.Kubernetes.BootstrapTokenRef)
	assert.Equal(t, "bootstrap-token-abc123", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
}

func Test_buildMachineCR_NilLabelsAndTaints(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Labels = nil
	cfg.Kubelet.RegisterWithTaints = nil
	machine := buildMachineCR(cfg)

	assert.Nil(t, machine.Spec.Kubernetes.NodeLabels)
	assert.Nil(t, machine.Spec.Kubernetes.RegisterWithTaints)
}

func Test_buildMachineCR_SiteLabelFromKubeletMachineSiteLabel(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Labels = map[string]string{
		"env":                        "test",
		v1alpha3.MachineSiteLabelKey: "site-a",
	}
	machine := buildMachineCR(cfg)

	require.NotNil(t, machine.Labels)
	assert.Equal(t, "site-a", machine.Labels[v1alpha3.MachineSiteLabelKey])
}

func Test_buildMachineCR_SiteLabelFromKubeletNetSiteLabel(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Labels = map[string]string{
		"env":                    "test",
		netv1alpha1.SiteLabelKey: "site-a",
	}
	machine := buildMachineCR(cfg)

	require.NotNil(t, machine.Labels)
	assert.Equal(t, "site-a", machine.Labels[v1alpha3.MachineSiteLabelKey])
}

func Test_buildMachineCR_MachineSiteLabelTakesPrecedence(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Labels = map[string]string{
		v1alpha3.MachineSiteLabelKey: "machine-site",
		netv1alpha1.SiteLabelKey:     "net-site",
	}
	machine := buildMachineCR(cfg)

	require.NotNil(t, machine.Labels)
	assert.Equal(t, "machine-site", machine.Labels[v1alpha3.MachineSiteLabelKey])
}

// ---------------------------------------------------------------------------
// registerMachine
// ---------------------------------------------------------------------------

func Test_registerMachine_EmptyToken_Skips(t *testing.T) {
	cfg := baseConfig()
	cfg.Kubelet.Auth.BootstrapToken = ""

	c := fakeClient()
	err := registerMachine(context.Background(), discardLogger(), c, cfg)
	require.NoError(t, err)

	// No Machine CR should have been created.
	var list v1alpha3.MachineList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items)
}

func Test_registerMachine_AlreadyExists_Skips(t *testing.T) {
	cfg := baseConfig()

	existing := buildMachineCR(cfg)
	c := fakeClient(&existing)

	err := registerMachine(context.Background(), discardLogger(), c, cfg)
	require.NoError(t, err)

	// Should still be exactly one Machine CR.
	var list v1alpha3.MachineList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Len(t, list.Items, 1)
}

func Test_registerMachine_NotFound_Creates(t *testing.T) {
	cfg := baseConfig()

	c := fakeClient()
	err := registerMachine(context.Background(), discardLogger(), c, cfg)
	require.NoError(t, err)

	// Verify Machine CR was created.
	var machine v1alpha3.Machine

	err = c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &machine)
	require.NoError(t, err)
	assert.Equal(t, "test-machine", machine.Name)
	require.NotNil(t, machine.Spec.Kubernetes.BootstrapTokenRef)
	assert.Equal(t, "bootstrap-token-abc123", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
}

func Test_registerMachine_Labels_Preserved(t *testing.T) {
	cfg := &provision.AgentConfig{
		MachineName: "labeled-machine",
		Kubelet: provision.AgentKubeletConfig{
			Auth: provision.KubeletAuthInfo{
				BootstrapToken: "tok123.secret",
			},
			Labels:             map[string]string{"env": "prod", "zone": "us-west"},
			RegisterWithTaints: []string{"gpu=true:NoSchedule"},
		},
	}

	c := fakeClient()
	err := registerMachine(context.Background(), discardLogger(), c, cfg)
	require.NoError(t, err)

	var machine v1alpha3.Machine

	err = c.Get(context.Background(), client.ObjectKey{Name: "labeled-machine"}, &machine)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod", "zone": "us-west"}, machine.Spec.Kubernetes.NodeLabels)
	assert.Equal(t, []string{"gpu=true:NoSchedule"}, machine.Spec.Kubernetes.RegisterWithTaints)
}
