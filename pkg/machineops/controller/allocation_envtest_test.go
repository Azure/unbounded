// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
	"github.com/Azure/unbounded/pkg/machineops/controller"
)

// TestEnvtestAllocationMinimalMain is the smallest custom provider controller
// that serves MachineAllocations: a leader-elected manager, a provider built
// only with pkg/machineops, AddToManager and AddCapacityToManager with the
// allocation controller enabled.
func TestEnvtestAllocationMinimalMain(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	const (
		providerName      = "ExampleCloud"
		siteName          = "site-a"
		configNamespace   = "capacity-config"
		leaderElectionID  = "example-cloud-site-a-allocation"
		leaseNamespace    = "default"
		configurationName = "workers"
		className         = "workers-spot"
		allocationName    = "alloc-1"
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
		// TestEnvtestCapacityMinimalMain registers the same controller names
		// in this test binary, and names are unique per process.
		Controller: config.Controller{SkipNameValidation: &[]bool{true}[0]},
	})
	require.NoError(t, err)

	owned := []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-native-1"}}

	var (
		mu             sync.Mutex
		allocateBegins []machineops.OperationRequest
		releaseBegins  []machineops.OperationRequest
	)

	resolveClass := func(context.Context, machineops.ClassRequest) (machineops.ClassResolution, error) {
		return machineops.ClassResolution{Offerings: []unboundedv1alpha3.MachineClassOffering{{
			Name:         "standard-x-spot",
			InstanceType: "Standard_X",
			CapacityType: unboundedv1alpha3.CapacityTypeSpot,
			Availability: unboundedv1alpha3.OfferingAvailable,
			Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "0.10"},
			ValidUntil:   &metav1.Time{Time: time.Now().Add(time.Hour)},
		}}}, nil
	}
	planAllocation := func(_ context.Context, request machineops.AllocationPlanRequest) (machineops.AllocationPlan, error) {
		return machineops.AllocationPlan{ProviderID: "example:///hosts/" + request.HostName, HostImage: "example-image"}, nil
	}
	beginAllocate := func(_ context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
		mu.Lock()
		defer mu.Unlock()

		allocateBegins = append(allocateBegins, request)

		return machineops.BeginResult{Operation: machineops.ProviderOperation{OperationID: "allocate-" + string(request.OperationUID)}}, nil
	}
	pollAllocate := func(context.Context, machineops.OperationRequest, machineops.ProviderOperation) (machineops.PollResult, error) {
		return machineops.PollResult{
			State: machineops.ProviderOperationStateSucceeded,
			Result: machineops.OperationResult{Allocation: &machineops.AllocationResult{
				ProviderResourceID: "vm-native-1",
				OwnedResources:     owned,
			}},
		}, nil
	}
	beginRelease := func(_ context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
		mu.Lock()
		defer mu.Unlock()

		releaseBegins = append(releaseBegins, request)

		return machineops.BeginResult{Operation: machineops.ProviderOperation{OperationID: "release-" + string(request.OperationUID)}}, nil
	}
	pollRelease := func(context.Context, machineops.OperationRequest, machineops.ProviderOperation) (machineops.PollResult, error) {
		return machineops.PollResult{State: machineops.ProviderOperationStateSucceeded}, nil
	}

	provider, err := machineops.NewProvider(
		providerName,
		machineops.WithCapacity(resolveClass, planAllocation),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, beginAllocate, pollAllocate),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, beginRelease, pollRelease),
	)
	require.NoError(t, err)

	providers := []*machineops.Provider{provider}

	require.NoError(t, controller.AddToManager(mgr, providers, controller.Options{
		ProviderName:              providerName,
		SiteName:                  siteName,
		ProviderPollInterval:      250 * time.Millisecond,
		CredentialSecretNamespace: "unbounded-system",
		ClusterInfo: &controller.ClusterInfo{
			APIServer:    "api.example.com:443",
			CACertBase64: "Y2E=",
			ClusterDNS:   "10.0.0.10",
			KubeVersion:  "v1.34.0",
		},
	}))

	require.NoError(t, controller.AddCapacityToManager(mgr, providers, controller.CapacityOptions{
		ProviderName:               providerName,
		SiteName:                   siteName,
		CredentialSecretNamespace:  "unbounded-system",
		ProviderConfigNamespace:    configNamespace,
		EnableAllocationController: true,
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
			Data:       map[string]string{"instanceType": "Standard_X"},
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

	var class unboundedv1alpha3.MachineClass

	require.Eventually(t, func() bool {
		if err := c.Get(ctx, client.ObjectKey{Name: className}, &class); err != nil {
			return false
		}

		return apimeta.IsStatusConditionTrue(class.Status.Conditions, unboundedv1alpha3.MachineClassConditionReady) && len(class.Status.Offerings) == 1
	}, time.Minute, 250*time.Millisecond, "MachineClass %s did not become Ready", className)

	allocation := &unboundedv1alpha3.MachineAllocation{
		ObjectMeta: metav1.ObjectMeta{Name: allocationName},
		Spec: unboundedv1alpha3.MachineAllocationSpec{
			ConsumerRef: unboundedv1alpha3.MachineAllocationConsumerReference{
				APIGroup: "karpenter.sh", Kind: "NodeClaim", Name: "default-x7k2p", UID: "6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10",
			},
			ClassRef: unboundedv1alpha3.UIDReference{Name: className, UID: class.UID},
		},
	}
	require.NoError(t, c.Create(ctx, allocation))

	machineName := "ub-" + strings.ReplaceAll(string(allocation.UID), "-", "")
	providerID := unboundedv1alpha3.AllocationProviderIDPrefix + string(allocation.UID)

	// The snapshot, the finalizer, the token and the bound Machine.
	require.Eventually(t, func() bool {
		if err := c.Get(ctx, client.ObjectKey{Name: allocationName}, allocation); err != nil {
			return false
		}

		return allocation.Status.Snapshot != nil && allocation.Status.Operations.Allocate != nil &&
			slices.Contains(allocation.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)
	}, time.Minute, 250*time.Millisecond, "MachineAllocation %s did not record its HostAllocate", allocationName)

	require.Equal(t, providerID, allocation.Status.ProviderID)
	require.Equal(t, "example:///hosts/"+machineName, allocation.Status.Snapshot.HostProviderID)

	var tokens corev1.SecretList
	require.NoError(t, c.List(ctx, &tokens, client.InNamespace(metav1.NamespaceSystem),
		client.MatchingLabels{unboundedv1alpha3.LabelAllocationUID: string(allocation.UID)}))
	require.Len(t, tokens.Items, 1)
	require.Equal(t, corev1.SecretTypeBootstrapToken, tokens.Items[0].Type)
	require.Equal(t, siteName, tokens.Items[0].Labels[unboundedv1alpha3.MachineSiteLabelKey])

	var machine unboundedv1alpha3.Machine
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: machineName}, &machine))
	require.Equal(t, &unboundedv1alpha3.UIDReference{Name: allocationName, UID: allocation.UID}, machine.Spec.AllocationRef)
	require.Nil(t, machine.Spec.Kubernetes.BootstrapTokenRef)

	// One Begin, with the providerID in its user data.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(allocateBegins) > 0
	}, time.Minute, 250*time.Millisecond, "HostAllocate did not begin")

	mu.Lock()
	require.Len(t, allocateBegins, 1)
	require.Equal(t, allocation.Status.Operations.Allocate.UID, allocateBegins[0].OperationUID)
	require.Contains(t, allocateBegins[0].Allocation.UserData, providerID)
	mu.Unlock()

	// The Node registers.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: machineName},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
	require.NoError(t, c.Create(ctx, node))

	require.Eventually(t, func() bool {
		if err := c.Get(ctx, client.ObjectKey{Name: allocationName}, allocation); err != nil {
			return false
		}

		if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			return false
		}

		return apimeta.IsStatusConditionTrue(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionAllocated) &&
			apimeta.IsStatusConditionTrue(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionRegistered) &&
			allocation.Status.NodeRef != nil && allocation.Status.NodeRef.UID == node.UID &&
			machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name == machineName
	}, time.Minute, 250*time.Millisecond, "MachineAllocation %s did not register its Node", allocationName)

	require.Equal(t, owned, allocation.Status.OwnedResources)

	// Release.
	require.NoError(t, c.Patch(ctx, allocation, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"lifecycle":"Released"}}`))))

	require.Eventually(t, func() bool {
		if err := c.Get(ctx, client.ObjectKey{Name: allocationName}, allocation); err != nil {
			return false
		}

		return apimeta.IsStatusConditionTrue(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionReleased) &&
			len(allocation.Finalizers) == 0
	}, time.Minute, 250*time.Millisecond, "MachineAllocation %s was not released", allocationName)

	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: machineName}, &machine))
	require.True(t, machine.Spec.Fenced)
	require.Empty(t, machine.Finalizers)

	err = c.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: tokens.Items[0].Name}, &corev1.Secret{})
	require.True(t, apierrors.IsNotFound(err), "bootstrap token Secret was not revoked: %v", err)

	for _, reference := range []*unboundedv1alpha3.UIDReference{allocation.Status.Operations.Allocate, allocation.Status.Operations.Release} {
		require.NotNil(t, reference)

		var operation unboundedv1alpha3.MachineOperation
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: reference.Name}, &operation))
		require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, operation.Status.Phase)
		require.Empty(t, operation.Finalizers)
	}

	mu.Lock()
	require.Len(t, releaseBegins, 1)
	require.Equal(t, owned, releaseBegins[0].Allocation.OwnedResources)
	require.Empty(t, releaseBegins[0].Allocation.UserData)
	mu.Unlock()
}
