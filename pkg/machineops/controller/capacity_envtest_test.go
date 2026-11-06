// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
	"github.com/Azure/unbounded/pkg/machineops/controller"
)

// TestEnvtestCapacityMinimalMain is the smallest custom provider controller
// that publishes MachineClass offerings: a leader-elected manager, a provider
// built only with pkg/machineops, and AddCapacityToManager.
func TestEnvtestCapacityMinimalMain(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	const (
		providerName      = "ExampleCloud"
		siteName          = "site-a"
		configNamespace   = "capacity-config"
		leaderElectionID  = "example-cloud-site-a-capacity"
		leaseNamespace    = "default"
		configurationName = "workers"
		className         = "workers-spot"
		offeringName      = "standard-x-spot"
		instanceType      = "Standard_X"
	)

	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, unboundedv1alpha3.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       leaseNamespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
	})
	require.NoError(t, err)

	var (
		mu       sync.Mutex
		requests []machineops.ClassRequest
	)

	resolveClass := func(_ context.Context, request machineops.ClassRequest) (machineops.ClassResolution, error) {
		mu.Lock()

		requests = append(requests, request)
		mu.Unlock()

		return machineops.ClassResolution{Offerings: []unboundedv1alpha3.MachineClassOffering{{
			Name:         offeringName,
			InstanceType: instanceType,
			CapacityType: unboundedv1alpha3.CapacityTypeSpot,
			Availability: unboundedv1alpha3.OfferingAvailable,
			Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "0.10"},
			ValidUntil:   &metav1.Time{Time: time.Now().Add(time.Hour)},
		}}}, nil
	}
	planAllocation := func(context.Context, machineops.AllocationPlanRequest) (machineops.AllocationPlan, error) {
		return machineops.AllocationPlan{}, nil
	}
	begin := func(context.Context, machineops.OperationRequest) (machineops.BeginResult, error) {
		return machineops.BeginResult{}, nil
	}
	poll := func(context.Context, machineops.OperationRequest, machineops.ProviderOperation) (machineops.PollResult, error) {
		return machineops.PollResult{}, nil
	}

	provider, err := machineops.NewProvider(
		providerName,
		machineops.WithCapacity(resolveClass, planAllocation),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, begin, poll),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, begin, poll),
	)
	require.NoError(t, err)

	require.NoError(t, controller.AddCapacityToManager(mgr, []*machineops.Provider{provider}, controller.CapacityOptions{
		ProviderName:              providerName,
		SiteName:                  siteName,
		CredentialSecretNamespace: "unbounded-system",
		ProviderConfigNamespace:   configNamespace,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	var site unboundedv1alpha3.Site
	require.NoError(t, json.Unmarshal([]byte(`{
		"metadata": {"name": "`+siteName+`"},
		"spec": {"nodeCidrs": ["10.0.0.0/16"], "podCidrAssignments": [{"cidrBlocks": ["10.244.0.0/16"]}]}
	}`), &site))

	version := int32(1)

	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: configNamespace}},
		&site,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: configNamespace, Name: "example-cloud-spot"},
			Immutable:  &[]bool{true}[0],
			Data:       map[string]string{"instanceType": instanceType},
		},
		&unboundedv1alpha3.MachineConfigurationVersion{
			ObjectMeta: metav1.ObjectMeta{
				Name:   unboundedv1alpha3.MachineConfigurationVersionName(configurationName, version),
				Labels: map[string]string{unboundedv1alpha3.MCVConfigurationLabelKey: configurationName},
			},
			Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
				Version: version,
				Template: unboundedv1alpha3.MachineConfigurationTemplate{
					Host: &unboundedv1alpha3.MachineConfigurationHostSpec{Image: "example-image"},
				},
			},
		},
		&unboundedv1alpha3.MachineOperationCredential{
			ObjectMeta: metav1.ObjectMeta{Name: "example-cloud-site-a"},
			Spec: unboundedv1alpha3.MachineOperationCredentialSpec{
				SiteName: siteName,
				Provider: providerName,
				Auth: unboundedv1alpha3.MachineOperationCredentialAuth{
					Mode: unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity,
				},
			},
		},
		&unboundedv1alpha3.MachineClass{
			ObjectMeta: metav1.ObjectMeta{Name: className},
			Spec: unboundedv1alpha3.MachineClassSpec{
				SiteRef:  unboundedv1alpha3.LocalObjectReference{Name: siteName},
				Provider: providerName,
				ProviderConfigRef: unboundedv1alpha3.TypedObjectReference{
					Kind: "ConfigMap", Namespace: configNamespace, Name: "example-cloud-spot",
				},
				ConfigurationRef: unboundedv1alpha3.MachineConfigurationRef{Name: configurationName, Version: &version},
			},
		},
	}
	for _, object := range objects {
		require.NoError(t, c.Create(ctx, object), "create %T %s", object, object.GetName())
	}

	require.Eventually(t, func() bool {
		var class unboundedv1alpha3.MachineClass
		if err := c.Get(ctx, client.ObjectKey{Name: className}, &class); err != nil {
			return false
		}

		ready := apimeta.FindStatusCondition(class.Status.Conditions, unboundedv1alpha3.MachineClassConditionReady)

		return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == class.Generation &&
			len(class.Status.Offerings) == 1 && class.Status.Offerings[0].Name == offeringName &&
			class.Status.Offerings[0].InstanceType == instanceType &&
			apimeta.IsStatusConditionTrue(class.Status.Conditions, unboundedv1alpha3.MachineClassConditionOfferingsCurrent)
	}, time.Minute, 250*time.Millisecond, "MachineClass %s did not become Ready with the provider's offerings", className)

	mu.Lock()
	require.NotEmpty(t, requests)
	require.Equal(t, siteName, requests[len(requests)-1].SiteName)
	require.Equal(t, map[string]string{"instanceType": instanceType}, requests[len(requests)-1].ProviderConfig.Data)
	require.Equal(t, unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity, requests[len(requests)-1].Auth.Mode)
	mu.Unlock()

	var lease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: leaseNamespace, Name: leaderElectionID}, &lease))
	require.NotNil(t, lease.Spec.HolderIdentity)
	require.NotEmpty(t, *lease.Spec.HolderIdentity)
}
