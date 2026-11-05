// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	classTestName            = "class-a"
	classTestProvider        = "ExampleCloud"
	classTestSite            = "site-a"
	classTestConfigNamespace = "capacity-config"
	classTestSecretNamespace = "unbounded-system"
	classTestConfiguration   = "workers"
	classTestSecretValue     = "secret-token-value"
	classTestConfigValue     = "provider-config-value"
)

var classTestNow = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

func TestMachineClassReconcilerResolvesClass(t *testing.T) {
	t.Parallel()

	fixture := newClassFixture()
	h := newClassHarness(t, fixture)
	h.resolver.setOfferings(
		classTestOffering("spot-b", unboundedv1alpha3.OfferingAvailable, 2*time.Minute),
		classTestOffering("on-demand-a", unboundedv1alpha3.OfferingUnavailable, 10*time.Minute),
	)

	result, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{RequeueAfter: 2 * time.Minute}, result)

	requests := h.resolver.requestsSnapshot()
	require.Len(t, requests, 1)
	request := requests[0]
	require.Equal(t, classTestName, request.ClassName)
	require.Equal(t, types.UID("class-a-uid"), request.ClassUID)
	require.Equal(t, classTestSite, request.SiteName)
	require.Equal(t, fixture.providerConfig.Data, request.ProviderConfig.Data)
	require.NotNil(t, request.Auth)
	require.Equal(t, unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin, request.Auth.Mode)
	require.Equal(t, map[string]string{"token": classTestSecretValue}, request.Auth.SecretData)

	status := h.class().Status
	require.Equal(t, int64(1), status.ObservedGeneration)

	providerConfig := &unboundedv1alpha3.ObjectContentSnapshot{
		Namespace: classTestConfigNamespace, Name: "provider-config", UID: "provider-config-uid", Hash: testJSONHash(t, fixture.providerConfig.Data),
	}
	bootstrapFiles := &unboundedv1alpha3.ObjectContentSnapshot{
		Namespace: classTestConfigNamespace, Name: "bootstrap-files", UID: "bootstrap-files-uid", Hash: testJSONHash(t, fixture.bootstrapFiles.Data),
	}
	configuration := &unboundedv1alpha3.ConfigurationContentSnapshot{
		Name: classTestConfiguration, Version: 1, VersionName: "workers-v1", UID: "workers-v1-uid", Hash: testJSONHash(t, fixture.version.Spec.Template),
	}
	site := &unboundedv1alpha3.UIDReference{Name: classTestSite, UID: "site-a-uid"}
	credential := &unboundedv1alpha3.UIDReference{Name: "cred-a", UID: "cred-a-uid"}

	require.Equal(t, providerConfig, status.ProviderConfig)
	require.Equal(t, bootstrapFiles, status.BootstrapFiles)
	require.Equal(t, configuration, status.Configuration)
	require.Equal(t, site, status.Site)
	require.Equal(t, credential, status.Credential)

	// The revision is the hash of one struct with this field order.
	expectedRevision := testJSONHash(t, struct {
		Provider       string                                          `json:"provider"`
		Site           *unboundedv1alpha3.UIDReference                 `json:"site"`
		Credential     *unboundedv1alpha3.UIDReference                 `json:"credential"`
		ProviderConfig *unboundedv1alpha3.ObjectContentSnapshot        `json:"providerConfig"`
		BootstrapFiles *unboundedv1alpha3.ObjectContentSnapshot        `json:"bootstrapFiles"`
		Configuration  *unboundedv1alpha3.ConfigurationContentSnapshot `json:"configuration"`
		AgentArtifact  *unboundedv1alpha3.AgentArtifact                `json:"agentArtifact"`
	}{classTestProvider, site, credential, providerConfig, bootstrapFiles, configuration, fixture.class.Spec.Bootstrap.AgentArtifact})
	require.Equal(t, expectedRevision, status.ResolvedRevision)

	require.Len(t, status.Offerings, 2)
	require.Equal(t, "on-demand-a", status.Offerings[0].Name)
	require.Equal(t, "spot-b", status.Offerings[1].Name)

	ready := requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved)
	require.Equal(t, int64(1), ready.ObservedGeneration)
	requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionTrue, classReasonOfferingsAvailable)
	require.Equal(t, 1, h.statusUpdateCount())
}

func TestMachineClassReconcilerNotReadyReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		reason               string
		classRefreshInterval time.Duration
		mutate               func(*testing.T, *classHarness)
		// forbiddenNamespace is a ConfigMap namespace that must never be read.
		forbiddenNamespace string
		resolveCalled      bool
	}{
		{
			name:   "site not found",
			reason: classReasonSiteNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.client.Delete(context.Background(), newClassTestSite("site-a-uid")))
			},
		},
		{
			name:                 "site not found with custom refresh interval",
			reason:               classReasonSiteNotFound,
			classRefreshInterval: 2 * time.Minute,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.client.Delete(context.Background(), newClassTestSite("site-a-uid")))
			},
		},
		{
			name:   "site deleting",
			reason: classReasonSiteDeleting,
			mutate: func(t *testing.T, h *classHarness) {
				var site unboundedv1alpha3.Site
				require.NoError(t, h.client.Get(context.Background(), client.ObjectKey{Name: classTestSite}, &site))
				site.Finalizers = []string{"example.com/hold"}
				require.NoError(t, h.client.Update(context.Background(), &site))
				require.NoError(t, h.client.Delete(context.Background(), &site))
			},
		},
		{
			name:   "provider config ref not a ConfigMap",
			reason: classReasonProviderConfigInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) { spec.ProviderConfigRef.Kind = "Secret" })
			},
			forbiddenNamespace: classTestConfigNamespace,
		},
		{
			name:   "provider config forbidden namespace",
			reason: classReasonProviderConfigForbidden,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) { spec.ProviderConfigRef.Namespace = "other" })
			},
			forbiddenNamespace: "other",
		},
		{
			name:   "provider config not found",
			reason: classReasonProviderConfigNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.configMaps.Delete(context.Background(), newClassFixture().providerConfig))
			},
		},
		{
			name:   "provider config not immutable",
			reason: classReasonProviderConfigNotImmutable,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().providerConfig
				configMap.Immutable = nil
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "provider config binary data",
			reason: classReasonProviderConfigInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().providerConfig
				configMap.BinaryData = map[string][]byte{"blob": []byte("x")}
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "bootstrap files forbidden namespace",
			reason: classReasonBootstrapFilesForbidden,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) { spec.Bootstrap.FilesRef.Namespace = "other" })
			},
			forbiddenNamespace: "other",
		},
		{
			name:   "bootstrap files not found",
			reason: classReasonBootstrapFilesNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.configMaps.Delete(context.Background(), newClassFixture().bootstrapFiles))
			},
		},
		{
			name:   "bootstrap files not immutable",
			reason: classReasonBootstrapFilesNotImmutable,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().bootstrapFiles
				configMap.Immutable = ptrTo(false)
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "bootstrap files binary data",
			reason: classReasonBootstrapFilesInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().bootstrapFiles
				configMap.BinaryData = map[string][]byte{"blob": []byte("x")}
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "bootstrap files missing run",
			reason: classReasonBootstrapFilesInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().bootstrapFiles
				configMap.Data = map[string]string{"other": "echo"}
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "bootstrap files blank run",
			reason: classReasonBootstrapFilesInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().bootstrapFiles
				configMap.Data = map[string]string{unboundedv1alpha3.MachineClassBootstrapRunKey: " \n\t"}
				h.replaceConfigMap(t, configMap)
			},
		},
		{
			name:   "configuration version unset",
			reason: classReasonConfigurationInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) { spec.ConfigurationRef.Version = nil })
			},
		},
		{
			name:   "configuration not found",
			reason: classReasonConfigurationNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.client.Delete(context.Background(), newClassFixture().version))
			},
		},
		{
			name:   "configuration version mismatch",
			reason: classReasonConfigurationMismatch,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateVersion(t, func(version *unboundedv1alpha3.MachineConfigurationVersion) { version.Spec.Version = 2 })
			},
		},
		{
			name:   "configuration label mismatch",
			reason: classReasonConfigurationMismatch,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateVersion(t, func(version *unboundedv1alpha3.MachineConfigurationVersion) {
					version.Labels[unboundedv1alpha3.MCVConfigurationLabelKey] = "other"
				})
			},
		},
		{
			name:   "credential not found",
			reason: authReasonNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.client.Delete(context.Background(), newClassFixture().credential))
			},
		},
		{
			name:   "credential ambiguous",
			reason: authReasonAmbiguous,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.client.Create(context.Background(), newWorkloadIdentityCredential("cred-b", classTestSite, classTestProvider)))
			},
		},
		{
			name:   "credential invalid",
			reason: authReasonInvalid,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateCredential(t, func(credential *unboundedv1alpha3.MachineOperationCredential) { credential.Spec.Auth.SecretRef = nil })
			},
		},
		{
			name:   "credential secret forbidden",
			reason: authReasonSecretForbidden,
			mutate: func(t *testing.T, h *classHarness) {
				h.updateCredential(t, func(credential *unboundedv1alpha3.MachineOperationCredential) {
					credential.Spec.Auth.SecretRef.Namespace = "other"
				})
			},
		},
		{
			name:   "credential secret not found",
			reason: authReasonSecretNotFound,
			mutate: func(t *testing.T, h *classHarness) {
				require.NoError(t, h.secrets.Delete(context.Background(), newClassFixture().secret))
			},
		},
		{
			name:   "provider rejects config",
			reason: classReasonProviderConfigRejected,
			mutate: func(_ *testing.T, h *classHarness) {
				h.resolver.setError(&publicmachineops.PermanentError{Reason: "BadSKU", Err: errors.New("sku Standard_X is not offered")})
			},
			resolveCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, newClassFixture())
			h.reconciler.ClassRefreshInterval = tt.classRefreshInterval
			h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

			_, err := h.reconcile()
			require.NoError(t, err)

			before := h.class().Status
			require.True(t, apimeta.IsStatusConditionTrue(before.Conditions, unboundedv1alpha3.MachineClassConditionReady))

			tt.mutate(t, h)
			h.resetConfigMapGets()

			result, err := h.reconcile()
			require.NoError(t, err)

			expectedRequeue := DefaultClassRefreshInterval
			if tt.classRefreshInterval > 0 {
				expectedRequeue = tt.classRefreshInterval
			}

			require.Equal(t, ctrl.Result{RequeueAfter: expectedRequeue}, result)

			class := h.class()
			status := class.Status
			ready := requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, tt.reason)
			require.Equal(t, class.Generation, ready.ObservedGeneration)
			require.NotContains(t, ready.Message, classTestSecretValue)
			require.NotContains(t, ready.Message, classTestConfigValue)

			if tt.reason == classReasonProviderConfigRejected {
				require.Contains(t, ready.Message, "sku Standard_X is not offered")
			}

			offeringsCurrent := requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonClassNotReady)
			require.Equal(t, class.Generation, offeringsCurrent.ObservedGeneration)

			requirePublishedTupleUnchanged(t, before, status)

			expectedCalls := 1
			if tt.resolveCalled {
				expectedCalls = 2
			}

			require.Len(t, h.resolver.requestsSnapshot(), expectedCalls)

			if tt.forbiddenNamespace != "" {
				for _, key := range h.configMapGetKeys() {
					require.NotEqual(t, tt.forbiddenNamespace, key.Namespace, "read ConfigMap %s", key)
				}
			}
		})
	}
}

func TestMachineClassReconcilerTransientErrorKeepsReady(t *testing.T) {
	t.Parallel()

	h := newClassHarness(t, newClassFixture())
	h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

	_, err := h.reconcile()
	require.NoError(t, err)

	before := h.class().Status
	readyBefore := apimeta.FindStatusCondition(before.Conditions, unboundedv1alpha3.MachineClassConditionReady)
	require.NotNil(t, readyBefore)

	h.resolver.setError(errors.New("provider API throttled"))
	h.reconciler.Now = func() time.Time { return classTestNow.Add(time.Minute) }

	result, err := h.reconcile()
	require.ErrorContains(t, err, "provider API throttled")
	require.Equal(t, ctrl.Result{}, result)

	status := h.class().Status
	require.Equal(t, *readyBefore, *apimeta.FindStatusCondition(status.Conditions, unboundedv1alpha3.MachineClassConditionReady))
	requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonResolveFailed)
	require.Equal(t, before.ObservedGeneration, status.ObservedGeneration)
	requirePublishedTupleUnchanged(t, before, status)
}

func TestMachineClassReconcilerTransientErrorAfterChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fixture    func() *classFixture
		change     func(*testing.T, *classHarness)
		generation int64
	}{
		{
			name:    "provider config replaced",
			fixture: newClassFixture,
			change: func(t *testing.T, h *classHarness) {
				configMap := newClassFixture().providerConfig
				configMap.UID = "provider-config-uid-2"
				configMap.Data = map[string]string{"sku": "Standard_Y"}
				h.replaceConfigMap(t, configMap)
			},
			generation: 1,
		},
		{
			name: "spec-only generation bump",
			fixture: func() *classFixture {
				fixture := newClassFixture()
				fixture.class.Spec.Bootstrap = nil

				return fixture
			},
			change: func(t *testing.T, h *classHarness) {
				h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) {
					spec.Bootstrap = &unboundedv1alpha3.MachineClassBootstrap{}
				})
			},
			generation: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, tt.fixture())
			h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

			_, err := h.reconcile()
			require.NoError(t, err)

			before := h.class().Status
			require.Equal(t, int64(1), before.ObservedGeneration)

			tt.change(t, h)
			h.resolver.setError(errors.New("provider API unavailable"))

			_, err = h.reconcile()
			require.Error(t, err)

			status := h.class().Status
			ready := requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, classReasonResolveFailed)
			require.Equal(t, tt.generation, ready.ObservedGeneration)
			requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonResolveFailed)
			require.Equal(t, int64(1), status.ObservedGeneration)
			requirePublishedTupleUnchanged(t, before, status)
		})
	}
}

func TestMachineClassReconcilerInvalidOfferings(t *testing.T) {
	t.Parallel()

	valid := func(name string) unboundedv1alpha3.MachineClassOffering {
		return classTestOffering(name, unboundedv1alpha3.OfferingAvailable, time.Hour)
	}

	tests := []struct {
		name      string
		offerings func() []unboundedv1alpha3.MachineClassOffering
	}{
		{name: "empty name", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			return []unboundedv1alpha3.MachineClassOffering{valid("")}
		}},
		{name: "duplicate name", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			return []unboundedv1alpha3.MachineClassOffering{valid("spot-a"), valid("spot-a")}
		}},
		{name: "empty instance type", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.InstanceType = ""

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
		{name: "unknown capacity type", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.CapacityType = "Reserved"

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
		{name: "unknown availability", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.Availability = "Maybe"

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
		{name: "missing validUntil", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.ValidUntil = nil

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
		{name: "bad currency", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.Price = &unboundedv1alpha3.OfferingPrice{Currency: "usd", PerHour: "0.10"}

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
		{name: "bad price per hour", offerings: func() []unboundedv1alpha3.MachineClassOffering {
			offering := valid("spot-a")
			offering.Price = &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "-1"}

			return []unboundedv1alpha3.MachineClassOffering{offering}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, newClassFixture())
			h.resolver.setOfferings(valid("on-demand-a"))

			_, err := h.reconcile()
			require.NoError(t, err)

			before := h.class().Status

			h.resolver.setOfferings(tt.offerings()...)

			result, err := h.reconcile()
			require.Error(t, err)
			require.Equal(t, ctrl.Result{}, result)

			status := h.class().Status
			requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved)
			requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonOfferingsInvalid)
			require.Equal(t, before.ObservedGeneration, status.ObservedGeneration)
			requirePublishedTupleUnchanged(t, before, status)
		})
	}

	t.Run("not ready after a change", func(t *testing.T) {
		t.Parallel()

		fixture := newClassFixture()
		h := newClassHarness(t, fixture)
		h.resolver.setOfferings(valid("on-demand-a"))

		_, err := h.reconcile()
		require.NoError(t, err)

		before := h.class().Status

		h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) {
			spec.Bootstrap.AgentArtifact.URL = "https://artifacts.example.com/agent-2.tar.gz"
		})
		h.resolver.setOfferings(valid(""))

		_, err = h.reconcile()
		require.Error(t, err)

		status := h.class().Status
		ready := requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, classReasonOfferingsInvalid)
		require.Equal(t, int64(2), ready.ObservedGeneration)
		require.Equal(t, int64(1), status.ObservedGeneration)
		requirePublishedTupleUnchanged(t, before, status)
	})
}

func TestMachineClassReconcilerNormalizesOfferings(t *testing.T) {
	t.Parallel()

	t.Run("sorted by name", func(t *testing.T) {
		t.Parallel()

		h := newClassHarness(t, newClassFixture())
		h.resolver.setOfferings(
			classTestOffering("c", unboundedv1alpha3.OfferingAvailable, time.Hour),
			classTestOffering("a", unboundedv1alpha3.OfferingAvailabilityUnknown, time.Hour),
			classTestOffering("b", unboundedv1alpha3.OfferingUnavailable, time.Hour),
		)

		_, err := h.reconcile()
		require.NoError(t, err)

		offerings := h.class().Status.Offerings
		require.Len(t, offerings, 3)
		require.Equal(t, []string{"a", "b", "c"}, []string{offerings[0].Name, offerings[1].Name, offerings[2].Name})
	})

	t.Run("zero offerings", func(t *testing.T) {
		t.Parallel()

		h := newClassHarness(t, newClassFixture())

		result, err := h.reconcile()
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: DefaultClassRefreshInterval}, result)

		status := h.class().Status
		require.Empty(t, status.Offerings)
		require.NotEmpty(t, status.ResolvedRevision)
		requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved)
		requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonNoOfferingsAvailable)
	})

	t.Run("deep copied", func(t *testing.T) {
		t.Parallel()

		offerings := []unboundedv1alpha3.MachineClassOffering{classTestOffering("a", unboundedv1alpha3.OfferingAvailable, time.Hour)}
		offerings[0].Labels = map[string]string{"zone": "1"}

		normalized, err := normalizeOfferings(offerings)
		require.NoError(t, err)

		offerings[0].Labels["zone"] = "2"
		offerings[0].ValidUntil.Time = classTestNow

		require.Equal(t, "1", normalized[0].Labels["zone"])
		require.Equal(t, classTestNow.Add(time.Hour), normalized[0].ValidUntil.Time)
	})
}

func TestMachineClassOfferingsCurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		offerings []unboundedv1alpha3.MachineClassOffering
		reason    string
		current   bool
	}{
		{
			name: "available and unexpired",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				classTestOffering("a", unboundedv1alpha3.OfferingUnavailable, time.Hour),
				classTestOffering("b", unboundedv1alpha3.OfferingAvailable, time.Hour),
			},
			reason:  classReasonOfferingsAvailable,
			current: true,
		},
		{
			name: "only unavailable and unknown",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				classTestOffering("a", unboundedv1alpha3.OfferingUnavailable, time.Hour),
				classTestOffering("b", unboundedv1alpha3.OfferingAvailabilityUnknown, time.Hour),
			},
			reason: classReasonNoOfferingsAvailable,
		},
		{
			name: "all expired",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				classTestOffering("a", unboundedv1alpha3.OfferingAvailable, -time.Minute),
				classTestOffering("b", unboundedv1alpha3.OfferingAvailable, 0),
			},
			reason: classReasonNoOfferingsAvailable,
		},
		{
			name:   "none",
			reason: classReasonNoOfferingsAvailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, newClassFixture())
			h.resolver.setOfferings(tt.offerings...)

			_, err := h.reconcile()
			require.NoError(t, err)

			status := metav1.ConditionFalse
			if tt.current {
				status = metav1.ConditionTrue
			}

			condition := requireClassCondition(t, h.class().Status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, status, tt.reason)
			require.Equal(t, int64(1), condition.ObservedGeneration)
		})
	}
}

func TestMachineClassRequeueAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		classRefreshInterval time.Duration
		minRequeueInterval   time.Duration
		validFor             []time.Duration
		want                 time.Duration
	}{
		{name: "default refresh interval", validFor: []time.Duration{time.Hour}, want: 5 * time.Minute},
		{name: "sooner validUntil", validFor: []time.Duration{time.Hour, 90 * time.Second}, want: 90 * time.Second},
		{name: "default floor", validFor: []time.Duration{2 * time.Second}, want: 10 * time.Second},
		{name: "custom floor raises", minRequeueInterval: 30 * time.Second, validFor: []time.Duration{15 * time.Second}, want: 30 * time.Second},
		{name: "custom floor lowers", minRequeueInterval: 2 * time.Second, validFor: []time.Duration{5 * time.Second}, want: 5 * time.Second},
		{name: "custom floor applies", minRequeueInterval: 2 * time.Second, validFor: []time.Duration{time.Second}, want: 2 * time.Second},
		{name: "custom refresh interval", classRefreshInterval: time.Minute, validFor: []time.Duration{time.Hour}, want: time.Minute},
		{name: "expired ignored", validFor: []time.Duration{-time.Minute, 0, 3 * time.Minute}, want: 3 * time.Minute},
		{name: "all expired", validFor: []time.Duration{-time.Minute}, want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, newClassFixture())
			h.reconciler.ClassRefreshInterval = tt.classRefreshInterval
			h.reconciler.MinRequeueInterval = tt.minRequeueInterval

			offerings := make([]unboundedv1alpha3.MachineClassOffering, 0, len(tt.validFor))
			for i, validFor := range tt.validFor {
				offerings = append(offerings, classTestOffering(string(rune('a'+i)), unboundedv1alpha3.OfferingAvailable, validFor))
			}

			h.resolver.setOfferings(offerings...)

			result, err := h.reconcile()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{RequeueAfter: tt.want}, result)
		})
	}
}

func TestMachineClassRevisionStableAcrossOfferingRefresh(t *testing.T) {
	t.Parallel()

	h := newClassHarness(t, newClassFixture())
	h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

	_, err := h.reconcile()
	require.NoError(t, err)

	first := h.class().Status

	later := classTestNow.Add(10 * time.Minute)
	h.reconciler.Now = func() time.Time { return later }

	refreshed := classTestOffering("spot-a", unboundedv1alpha3.OfferingUnavailable, 2*time.Hour)
	refreshed.ObservedAt = &metav1.Time{Time: later}
	refreshed.Price = &unboundedv1alpha3.OfferingPrice{Currency: "EUR", PerHour: "0.42", ObservedAt: &metav1.Time{Time: later}}
	h.resolver.setOfferings(refreshed, classTestOffering("on-demand-b", unboundedv1alpha3.OfferingAvailable, 3*time.Hour))

	_, err = h.reconcile()
	require.NoError(t, err)

	second := h.class().Status
	require.NotEqual(t, first.Offerings, second.Offerings)
	require.NotEmpty(t, first.ResolvedRevision)
	require.Equal(t, first.ResolvedRevision, second.ResolvedRevision)
}

func TestMachineClassRevisionChangesWithLaunchInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*testing.T, *classHarness)
	}{
		{name: "site UID", change: func(t *testing.T, h *classHarness) {
			require.NoError(t, h.client.Delete(context.Background(), newClassTestSite("site-a-uid")))
			require.NoError(t, h.client.Create(context.Background(), newClassTestSite("site-a-uid-2")))
		}},
		{name: "credential UID", change: func(t *testing.T, h *classHarness) {
			credential := newClassFixture().credential
			require.NoError(t, h.client.Delete(context.Background(), credential))

			credential.UID = "cred-a-uid-2"
			credential.ResourceVersion = ""
			require.NoError(t, h.client.Create(context.Background(), credential))
		}},
		{name: "provider config data", change: func(t *testing.T, h *classHarness) {
			configMap := newClassFixture().providerConfig
			configMap.Data["sku"] = "Standard_Y"
			h.replaceConfigMap(t, configMap)
		}},
		{name: "provider config UID", change: func(t *testing.T, h *classHarness) {
			configMap := newClassFixture().providerConfig
			configMap.UID = "provider-config-uid-2"
			h.replaceConfigMap(t, configMap)
		}},
		{name: "bootstrap data", change: func(t *testing.T, h *classHarness) {
			configMap := newClassFixture().bootstrapFiles
			configMap.Data[unboundedv1alpha3.MachineClassBootstrapRunKey] = "echo changed"
			h.replaceConfigMap(t, configMap)
		}},
		{name: "bootstrap files presence", change: func(t *testing.T, h *classHarness) {
			h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) { spec.Bootstrap.FilesRef = nil })
		}},
		{name: "configuration template", change: func(t *testing.T, h *classHarness) {
			h.updateVersion(t, func(version *unboundedv1alpha3.MachineConfigurationVersion) {
				version.Spec.Template.Host.Image = "image-2"
			})
		}},
		{name: "configuration version UID", change: func(t *testing.T, h *classHarness) {
			version := newClassFixture().version
			require.NoError(t, h.client.Delete(context.Background(), version))

			version.UID = "workers-v1-uid-2"
			version.ResourceVersion = ""
			require.NoError(t, h.client.Create(context.Background(), version))
		}},
		{name: "agent artifact URL", change: func(t *testing.T, h *classHarness) {
			h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) {
				spec.Bootstrap.AgentArtifact.URL = "https://artifacts.example.com/agent-2.tar.gz"
			})
		}},
		{name: "agent artifact digest", change: func(t *testing.T, h *classHarness) {
			h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) {
				spec.Bootstrap.AgentArtifact.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newClassHarness(t, newClassFixture())
			h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

			_, err := h.reconcile()
			require.NoError(t, err)

			before := h.class().Status.ResolvedRevision
			require.NotEmpty(t, before)

			tt.change(t, h)

			_, err = h.reconcile()
			require.NoError(t, err)

			status := h.class().Status
			requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved)
			require.NotEqual(t, before, status.ResolvedRevision)
		})
	}
}

func TestMachineClassReconcilerIgnoresOutOfScopeClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		siteName string
	}{
		{name: "provider not registered", provider: "OtherCloud"},
		{name: "provider without capacity", provider: "PlainCloud"},
		{name: "other site", provider: classTestProvider, siteName: "site-b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newClassFixture()
			fixture.class.Spec.Provider = tt.provider
			fixture.credential.Spec.Provider = tt.provider

			h := newClassHarness(t, fixture)
			h.reconciler.Providers = append(h.reconciler.Providers, newClassTestPlainProvider(t, "PlainCloud"))
			h.reconciler.SiteName = tt.siteName

			result, err := h.reconcile()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
			h.requireOnlyClassRead(t)
		})
	}
}

func TestMachineClassReconcilerIgnoresDeletingClass(t *testing.T) {
	t.Parallel()

	t.Run("deleting", func(t *testing.T) {
		t.Parallel()

		fixture := newClassFixture()
		fixture.class.Finalizers = []string{"example.com/hold"}

		h := newClassHarness(t, fixture)
		require.NoError(t, h.client.Delete(context.Background(), h.class()))
		require.False(t, h.class().DeletionTimestamp.IsZero())
		h.resetMainReads()

		result, err := h.reconcile()
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, result)
		h.requireOnlyClassRead(t)
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		h := newClassHarness(t, newClassFixture())
		require.NoError(t, h.client.Delete(context.Background(), h.class()))
		h.resetMainReads()

		result, err := h.reconcile()
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, result)
		h.requireOnlyClassRead(t)
	})
}

func TestMachineClassReconcilerStaleReadConflicts(t *testing.T) {
	t.Parallel()

	h := newClassHarness(t, newClassFixture())
	h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

	_, err := h.reconcile()
	require.NoError(t, err)

	stale := h.class()

	h.updateClassSpec(t, func(spec *unboundedv1alpha3.MachineClassSpec) {
		spec.Bootstrap.AgentArtifact.URL = "https://artifacts.example.com/agent-2.tar.gz"
	})

	_, err = h.reconcile()
	require.NoError(t, err)

	current := h.class()
	require.Equal(t, int64(2), current.Status.ObservedGeneration)
	require.Equal(t, int64(2), apimeta.FindStatusCondition(current.Status.Conditions, unboundedv1alpha3.MachineClassConditionReady).ObservedGeneration)

	// A deposed leader whose cache still holds generation 1 resolves new
	// offerings and tries to publish them.
	h.serveStaleClass(stale)
	h.resolver.setOfferings(classTestOffering("spot-b", unboundedv1alpha3.OfferingAvailable, 2*time.Hour))

	_, err = h.reconcile()
	require.Error(t, err)
	require.True(t, apierrors.IsConflict(err), "expected a conflict, got %v", err)

	after := h.class()
	require.Equal(t, current.Status, after.Status)
	require.Equal(t, int64(2), after.Status.ObservedGeneration)
	require.Equal(t, int64(2), apimeta.FindStatusCondition(after.Status.Conditions, unboundedv1alpha3.MachineClassConditionReady).ObservedGeneration)
}

func TestMachineClassReconcilerSkipsUnchangedStatus(t *testing.T) {
	t.Parallel()

	h := newClassHarness(t, newClassFixture())
	h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

	_, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 1, h.statusUpdateCount())

	_, err = h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 1, h.statusUpdateCount())

	// An input failure that is already published is not written again either.
	require.NoError(t, h.client.Delete(context.Background(), newClassTestSite("site-a-uid")))

	_, err = h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 2, h.statusUpdateCount())

	_, err = h.reconcile()
	require.NoError(t, err)
	require.Equal(t, 2, h.statusUpdateCount())
}

func TestMachineClassReconcilerRecoversWhenInputAppears(t *testing.T) {
	t.Parallel()

	fixture := newClassFixture()
	providerConfig := fixture.providerConfig
	fixture.providerConfig = nil

	h := newClassHarness(t, fixture)
	h.resolver.setOfferings(classTestOffering("spot-a", unboundedv1alpha3.OfferingAvailable, time.Hour))

	result, err := h.reconcile()
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{RequeueAfter: DefaultClassRefreshInterval}, result)

	status := h.class().Status
	requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, classReasonProviderConfigNotFound)
	require.Empty(t, status.ResolvedRevision)
	require.Empty(t, h.resolver.requestsSnapshot())

	require.NoError(t, h.configMaps.Create(context.Background(), providerConfig))

	_, err = h.reconcile()
	require.NoError(t, err)

	status = h.class().Status
	requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved)
	requireClassCondition(t, status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionTrue, classReasonOfferingsAvailable)
	require.NotEmpty(t, status.ResolvedRevision)
	require.Equal(t, types.UID("provider-config-uid"), status.ProviderConfig.UID)
	require.Equal(t, int64(1), status.ObservedGeneration)
}

func TestMachineClassWatchMapping(t *testing.T) {
	t.Parallel()

	newClass := func(name, site, provider, providerConfig, configuration string) *unboundedv1alpha3.MachineClass {
		class := newClassFixture().class
		class.Name = name
		class.UID = types.UID(name + "-uid")
		class.Spec.SiteRef.Name = site
		class.Spec.Provider = provider
		class.Spec.ProviderConfigRef.Name = providerConfig
		class.Spec.ConfigurationRef.Name = configuration

		return class
	}

	matching := newClass("matching", classTestSite, classTestProvider, "provider-config", classTestConfiguration)
	other := newClass("other", classTestSite, classTestProvider, "other-config", "other")
	other.Spec.Bootstrap = nil
	otherSite := newClass("other-site", "site-b", classTestProvider, "provider-config", classTestConfiguration)
	otherProvider := newClass("other-provider", classTestSite, "OtherCloud", "provider-config", classTestConfiguration)

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).
		WithObjects(matching, other, otherSite, otherProvider).Build()
	reconciler := &MachineClassReconciler{
		Client:    c,
		Providers: []*Provider{newClassTestProvider(t, classTestProvider, &fakeClassResolver{})},
		SiteName:  classTestSite,
	}

	ctx := context.Background()
	names := func(requests []reconcile.Request) []string {
		result := make([]string, 0, len(requests))
		for _, request := range requests {
			require.Empty(t, request.Namespace)
			result = append(result, request.Name)
		}

		return result
	}

	labeledVersion := func(configuration string) *unboundedv1alpha3.MachineConfigurationVersion {
		version := newClassFixture().version
		version.Labels = map[string]string{unboundedv1alpha3.MCVConfigurationLabelKey: configuration}

		return version
	}

	configMap := func(namespace, name string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	}

	require.ElementsMatch(t, []string{"matching", "other"}, names(reconciler.classesForSite(ctx, newClassTestSite("site-a-uid"))))
	require.Empty(t, reconciler.classesForSite(ctx, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}}))

	require.ElementsMatch(t, []string{"matching"}, names(reconciler.classesForConfigurationVersion(ctx, labeledVersion(classTestConfiguration))))
	require.ElementsMatch(t, []string{"other"}, names(reconciler.classesForConfigurationVersion(ctx, labeledVersion("other"))))
	require.Empty(t, reconciler.classesForConfigurationVersion(ctx, &unboundedv1alpha3.MachineConfigurationVersion{ObjectMeta: metav1.ObjectMeta{Name: "workers-v1"}}))

	require.ElementsMatch(t, []string{"matching", "other"}, names(reconciler.classesForCredential(ctx, newWorkloadIdentityCredential("c", classTestSite, classTestProvider))))
	require.Empty(t, reconciler.classesForCredential(ctx, newWorkloadIdentityCredential("c", classTestSite, "OtherCloud")))
	require.Empty(t, reconciler.classesForCredential(ctx, newWorkloadIdentityCredential("c", "site-b", classTestProvider)))

	require.ElementsMatch(t, []string{"matching"}, names(reconciler.classesForConfigMap(ctx, configMap(classTestConfigNamespace, "provider-config"))))
	require.ElementsMatch(t, []string{"matching"}, names(reconciler.classesForConfigMap(ctx, configMap(classTestConfigNamespace, "bootstrap-files"))))
	require.ElementsMatch(t, []string{"other"}, names(reconciler.classesForConfigMap(ctx, configMap(classTestConfigNamespace, "other-config"))))
	require.Empty(t, reconciler.classesForConfigMap(ctx, configMap("other", "provider-config")))
}

func TestMachineClassEventPredicate(t *testing.T) {
	t.Parallel()

	reconciler := &MachineClassReconciler{
		Providers: []*Provider{newClassTestProvider(t, classTestProvider, &fakeClassResolver{})},
		SiteName:  classTestSite,
	}
	predicate := reconciler.classPredicate()

	inScope := newClassFixture().class
	outOfScope := newClassFixture().class
	outOfScope.Spec.SiteRef.Name = "site-b"

	bumped := inScope.DeepCopy()
	bumped.Generation++

	statusOnly := inScope.DeepCopy()
	statusOnly.Status.ResolvedRevision = "changed"

	require.True(t, predicate.Create(event.CreateEvent{Object: inScope}))
	require.False(t, predicate.Create(event.CreateEvent{Object: outOfScope}))
	require.True(t, predicate.Update(event.UpdateEvent{ObjectOld: inScope, ObjectNew: bumped}))
	require.False(t, predicate.Update(event.UpdateEvent{ObjectOld: inScope, ObjectNew: statusOnly}))
	require.True(t, predicate.Delete(event.DeleteEvent{Object: inScope}))
	require.False(t, predicate.Delete(event.DeleteEvent{Object: outOfScope}))
	require.True(t, predicate.Generic(event.GenericEvent{Object: inScope}))
	require.False(t, predicate.Generic(event.GenericEvent{Object: outOfScope}))
}

// classFixture is one in-scope MachineClass and every input it references.
type classFixture struct {
	class          *unboundedv1alpha3.MachineClass
	site           *unboundedv1alpha3.Site
	providerConfig *corev1.ConfigMap
	bootstrapFiles *corev1.ConfigMap
	version        *unboundedv1alpha3.MachineConfigurationVersion
	credential     *unboundedv1alpha3.MachineOperationCredential
	secret         *corev1.Secret
}

func newClassFixture() *classFixture {
	credential := newMachineOperationCredential("cred-a", classTestSite, classTestProvider)
	credential.UID = "cred-a-uid"

	return &classFixture{
		class: &unboundedv1alpha3.MachineClass{
			ObjectMeta: metav1.ObjectMeta{Name: classTestName, UID: "class-a-uid", Generation: 1},
			Spec: unboundedv1alpha3.MachineClassSpec{
				SiteRef:  unboundedv1alpha3.LocalObjectReference{Name: classTestSite},
				Provider: classTestProvider,
				ProviderConfigRef: unboundedv1alpha3.TypedObjectReference{
					Kind: "ConfigMap", Namespace: classTestConfigNamespace, Name: "provider-config",
				},
				ConfigurationRef: unboundedv1alpha3.MachineConfigurationRef{Name: classTestConfiguration, Version: ptrTo(int32(1))},
				Bootstrap: &unboundedv1alpha3.MachineClassBootstrap{
					FilesRef: &unboundedv1alpha3.NamespacedObjectReference{Namespace: classTestConfigNamespace, Name: "bootstrap-files"},
					AgentArtifact: &unboundedv1alpha3.AgentArtifact{
						URL:    "https://artifacts.example.com/agent.tar.gz",
						SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					},
				},
			},
		},
		site: newClassTestSite("site-a-uid"),
		providerConfig: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: classTestConfigNamespace, Name: "provider-config", UID: "provider-config-uid"},
			Immutable:  ptrTo(true),
			Data:       map[string]string{"sku": "Standard_X", "token": classTestConfigValue},
		},
		bootstrapFiles: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: classTestConfigNamespace, Name: "bootstrap-files", UID: "bootstrap-files-uid"},
			Immutable:  ptrTo(true),
			Data:       map[string]string{unboundedv1alpha3.MachineClassBootstrapRunKey: "echo hello"},
		},
		version: &unboundedv1alpha3.MachineConfigurationVersion{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "workers-v1",
				UID:    "workers-v1-uid",
				Labels: map[string]string{unboundedv1alpha3.MCVConfigurationLabelKey: classTestConfiguration},
			},
			Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
				Version: 1,
				Template: unboundedv1alpha3.MachineConfigurationTemplate{
					Host: &unboundedv1alpha3.MachineConfigurationHostSpec{Image: "image-1"},
				},
			},
		},
		credential: credential,
		secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: classTestSecretNamespace, Name: "cred-a"},
			Data:       map[string][]byte{"token": []byte(classTestSecretValue)},
		},
	}
}

func newClassTestSite(uid types.UID) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: classTestSite, UID: uid}}
}

// classHarness runs a MachineClassReconciler against three separate fake
// clients: the main client holds no ConfigMaps or Secrets, so a read through
// the wrong reader fails.
type classHarness struct {
	client     client.Client
	configMaps client.Client
	secrets    client.Client
	resolver   *fakeClassResolver
	reconciler *MachineClassReconciler

	mu            sync.Mutex
	statusUpdates int
	mainReads     []string
	configMapGets []client.ObjectKey
	secretGets    int
	staleClass    *unboundedv1alpha3.MachineClass
}

func newClassHarness(t *testing.T, fixture *classFixture) *classHarness {
	t.Helper()

	h := &classHarness{resolver: &fakeClassResolver{}}

	h.client = fake.NewClientBuilder().
		WithScheme(newOperationTestScheme(t)).
		WithObjects(fixture.class, fixture.site, fixture.version, fixture.credential).
		WithStatusSubresource(&unboundedv1alpha3.MachineClass{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				h.mu.Lock()
				h.mainReads = append(h.mainReads, readKind(obj))
				stale := h.staleClass

				class, isClass := obj.(*unboundedv1alpha3.MachineClass)
				if isClass {
					h.staleClass = nil
				}
				h.mu.Unlock()

				if isClass && stale != nil {
					stale.DeepCopyInto(class)
					return nil
				}

				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				h.mu.Lock()
				h.mainReads = append(h.mainReads, readKind(list))
				h.mu.Unlock()

				return c.List(ctx, list, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				h.mu.Lock()
				h.statusUpdates++
				h.mu.Unlock()

				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()

	coreScheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(coreScheme))

	var configMaps []client.Object

	for _, configMap := range []*corev1.ConfigMap{fixture.providerConfig, fixture.bootstrapFiles} {
		if configMap != nil {
			configMaps = append(configMaps, configMap)
		}
	}

	h.configMaps = fake.NewClientBuilder().
		WithScheme(coreScheme).
		WithObjects(configMaps...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				h.mu.Lock()
				h.configMapGets = append(h.configMapGets, key)
				h.mu.Unlock()

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	var secrets []client.Object
	if fixture.secret != nil {
		secrets = append(secrets, fixture.secret)
	}

	h.secrets = fake.NewClientBuilder().
		WithScheme(coreScheme).
		WithObjects(secrets...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				h.mu.Lock()
				h.secretGets++
				h.mu.Unlock()

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	h.reconciler = &MachineClassReconciler{
		Client:                    h.client,
		ConfigMapReader:           h.configMaps,
		SecretReader:              h.secrets,
		Providers:                 []*Provider{newClassTestProvider(t, classTestProvider, h.resolver)},
		CredentialSecretNamespace: classTestSecretNamespace,
		ProviderConfigNamespace:   classTestConfigNamespace,
		Now:                       func() time.Time { return classTestNow },
	}

	return h
}

func readKind(object runtime.Object) string {
	switch object.(type) {
	case *unboundedv1alpha3.MachineClass:
		return "MachineClass"
	case *unboundedv1alpha3.MachineClassList:
		return "MachineClassList"
	default:
		return "other"
	}
}

func (h *classHarness) reconcile() (ctrl.Result, error) {
	return h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: classTestName}})
}

func (h *classHarness) class() *unboundedv1alpha3.MachineClass {
	var class unboundedv1alpha3.MachineClass
	if err := h.client.Get(context.Background(), client.ObjectKey{Name: classTestName}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &class
}

func (h *classHarness) statusUpdateCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.statusUpdates
}

func (h *classHarness) configMapGetKeys() []client.ObjectKey {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]client.ObjectKey(nil), h.configMapGets...)
}

func (h *classHarness) resetConfigMapGets() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.configMapGets = nil
}

func (h *classHarness) resetMainReads() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.mainReads = nil
}

func (h *classHarness) serveStaleClass(class *unboundedv1alpha3.MachineClass) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.staleClass = class.DeepCopy()
}

// requireOnlyClassRead asserts that the reconcile read only the class: no
// other object, no ConfigMap or Secret, no ResolveClass call and no write.
func (h *classHarness) requireOnlyClassRead(t *testing.T) {
	t.Helper()

	h.mu.Lock()
	defer h.mu.Unlock()

	require.Equal(t, []string{"MachineClass"}, h.mainReads)
	require.Empty(t, h.configMapGets)
	require.Zero(t, h.secretGets)
	require.Zero(t, h.statusUpdates)
	require.Empty(t, h.resolver.requestsSnapshot())
}

func (h *classHarness) updateClassSpec(t *testing.T, mutate func(*unboundedv1alpha3.MachineClassSpec)) {
	t.Helper()

	class := h.class()
	mutate(&class.Spec)
	class.Generation++
	require.NoError(t, h.client.Update(context.Background(), class))
}

func (h *classHarness) updateVersion(t *testing.T, mutate func(*unboundedv1alpha3.MachineConfigurationVersion)) {
	t.Helper()

	var version unboundedv1alpha3.MachineConfigurationVersion
	require.NoError(t, h.client.Get(context.Background(), client.ObjectKey{Name: "workers-v1"}, &version))
	mutate(&version)
	require.NoError(t, h.client.Update(context.Background(), &version))
}

func (h *classHarness) updateCredential(t *testing.T, mutate func(*unboundedv1alpha3.MachineOperationCredential)) {
	t.Helper()

	var credential unboundedv1alpha3.MachineOperationCredential
	require.NoError(t, h.client.Get(context.Background(), client.ObjectKey{Name: "cred-a"}, &credential))
	mutate(&credential)
	require.NoError(t, h.client.Update(context.Background(), &credential))
}

// replaceConfigMap deletes and recreates a ConfigMap, the only way to change
// an immutable one.
func (h *classHarness) replaceConfigMap(t *testing.T, configMap *corev1.ConfigMap) {
	t.Helper()

	err := h.configMaps.Delete(context.Background(), configMap.DeepCopy())
	if !apierrors.IsNotFound(err) {
		require.NoError(t, err)
	}

	configMap.ResourceVersion = ""
	require.NoError(t, h.configMaps.Create(context.Background(), configMap))
}

// fakeClassResolver records ResolveClass requests and returns a configured
// resolution or error.
type fakeClassResolver struct {
	mu         sync.Mutex
	requests   []publicmachineops.ClassRequest
	resolution publicmachineops.ClassResolution
	err        error
}

func (f *fakeClassResolver) ResolveClass(_ context.Context, request publicmachineops.ClassRequest) (publicmachineops.ClassResolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, request)
	if f.err != nil {
		return publicmachineops.ClassResolution{}, f.err
	}

	offerings := make([]unboundedv1alpha3.MachineClassOffering, len(f.resolution.Offerings))
	for i := range f.resolution.Offerings {
		f.resolution.Offerings[i].DeepCopyInto(&offerings[i])
	}

	return publicmachineops.ClassResolution{Offerings: offerings}, nil
}

func (f *fakeClassResolver) setOfferings(offerings ...unboundedv1alpha3.MachineClassOffering) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resolution = publicmachineops.ClassResolution{Offerings: offerings}
	f.err = nil
}

func (f *fakeClassResolver) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.err = err
}

func (f *fakeClassResolver) requestsSnapshot() []publicmachineops.ClassRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]publicmachineops.ClassRequest(nil), f.requests...)
}

func newClassTestProvider(t *testing.T, name string, resolver *fakeClassResolver) *Provider {
	t.Helper()

	recorder := &allocationCallRecorder{}

	provider, err := publicmachineops.NewProvider(
		name,
		publicmachineops.WithCapacity(resolver.ResolveClass, recorder.PlanAllocation),
		publicmachineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, recorder.Begin, recorder.Poll),
		publicmachineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, recorder.Begin, recorder.Poll),
	)
	require.NoError(t, err)

	return provider
}

func newClassTestPlainProvider(t *testing.T, name string) *Provider {
	t.Helper()

	recorder := &allocationCallRecorder{}

	provider, err := publicmachineops.NewProvider(
		name,
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, recorder.Execute),
	)
	require.NoError(t, err)

	return provider
}

func classTestOffering(name string, availability unboundedv1alpha3.OfferingAvailability, validFor time.Duration) unboundedv1alpha3.MachineClassOffering {
	return unboundedv1alpha3.MachineClassOffering{
		Name:         name,
		InstanceType: "Standard_X",
		CapacityType: unboundedv1alpha3.CapacityTypeSpot,
		Availability: availability,
		Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "0.10"},
		ValidUntil:   &metav1.Time{Time: classTestNow.Add(validFor)},
	}
}

func requireClassCondition(
	t *testing.T,
	status unboundedv1alpha3.MachineClassStatus,
	conditionType string,
	conditionStatus metav1.ConditionStatus,
	reason string,
) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(status.Conditions, conditionType)
	require.NotNil(t, condition, "condition %s", conditionType)
	require.Equal(t, conditionStatus, condition.Status, "condition %s: %s", conditionType, condition.Message)
	require.Equal(t, reason, condition.Reason, "condition %s: %s", conditionType, condition.Message)

	return condition
}

// requirePublishedTupleUnchanged asserts that the revision, the snapshots,
// the references and the offerings were kept.
func requirePublishedTupleUnchanged(t *testing.T, before, after unboundedv1alpha3.MachineClassStatus) {
	t.Helper()

	require.NotEmpty(t, before.ResolvedRevision)
	require.Equal(t, before.ResolvedRevision, after.ResolvedRevision)
	require.Equal(t, before.ProviderConfig, after.ProviderConfig)
	require.Equal(t, before.BootstrapFiles, after.BootstrapFiles)
	require.Equal(t, before.Configuration, after.Configuration)
	require.Equal(t, before.Site, after.Site)
	require.Equal(t, before.Credential, after.Credential)
	require.Equal(t, before.Offerings, after.Offerings)
}

func testJSONHash(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}
