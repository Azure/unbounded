// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/racer/injector"
	"github.com/Azure/unbounded/internal/version"
)

func TestMutatingWebhookManifest(t *testing.T) {
	data, err := fs.ReadFile(Manifests, "mutating-webhook.yaml")
	require.NoError(t, err)

	var config admissionregistrationv1.MutatingWebhookConfiguration
	require.NoError(t, yaml.UnmarshalStrict(data, &config))
	require.Equal(t, "MutatingWebhookConfiguration", config.Kind)
	require.Equal(t, "racer-object-sidecar-injector", config.Name)
	require.Len(t, config.Webhooks, 1)

	hook := config.Webhooks[0]
	require.Equal(t, "racer-object-sidecar.racer.unbounded-cloud.io", hook.Name)
	require.Empty(t, hook.ClientConfig.CABundle)
	require.Nil(t, hook.ClientConfig.URL)
	require.Equal(t, &admissionregistrationv1.ServiceReference{Name: "racer-controller", Namespace: "unbounded-system", Path: ptr(injector.Path), Port: ptr(int32(9443))}, hook.ClientConfig.Service)

	require.Len(t, hook.Rules, 1)
	rule := hook.Rules[0]
	require.Equal(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}, rule.Operations)
	require.Equal(t, []string{""}, rule.APIGroups)
	require.Equal(t, []string{"v1"}, rule.APIVersions)
	require.Equal(t, []string{"pods"}, rule.Resources)
	require.Equal(t, admissionregistrationv1.NamespacedScope, *rule.Scope)

	require.Equal(t, map[string]string{injector.EnabledLabel: injector.EnabledValue}, hook.ObjectSelector.MatchLabels)
	require.Empty(t, hook.ObjectSelector.MatchExpressions)
	require.Empty(t, hook.NamespaceSelector.MatchLabels)
	require.Len(t, hook.NamespaceSelector.MatchExpressions, 1)
	require.Equal(t, "kubernetes.io/metadata.name", hook.NamespaceSelector.MatchExpressions[0].Key)
	require.EqualValues(t, "NotIn", hook.NamespaceSelector.MatchExpressions[0].Operator)
	require.Equal(t, []string{"unbounded-system"}, hook.NamespaceSelector.MatchExpressions[0].Values)

	require.Len(t, hook.MatchConditions, 1)
	require.Equal(t, "not-already-injected", hook.MatchConditions[0].Name)
	require.Equal(t, 2, strings.Count(hook.MatchConditions[0].Expression, "c.name == '"+injector.ContainerName+"'"))

	require.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy)
	require.Equal(t, admissionregistrationv1.Equivalent, *hook.MatchPolicy)
	require.Equal(t, admissionregistrationv1.SideEffectClassNone, *hook.SideEffects)
	require.Equal(t, admissionregistrationv1.NeverReinvocationPolicy, *hook.ReinvocationPolicy)
	require.Equal(t, int32(5), *hook.TimeoutSeconds)
	require.Equal(t, []string{"v1"}, hook.AdmissionReviewVersions)
}

func TestControllerExposesWebhookPort(t *testing.T) {
	data, err := fs.ReadFile(Manifests, "controller.yaml")
	require.NoError(t, err)

	documents := strings.Split(string(data), "\n---\n")
	require.Len(t, documents, 2)

	var deployment appsv1.Deployment
	require.NoError(t, yaml.Unmarshal([]byte(documents[0]), &deployment))
	require.Contains(t, deployment.Spec.Template.Spec.Containers[0].Ports, corev1.ContainerPort{Name: "webhook", ContainerPort: 9443, Protocol: corev1.ProtocolTCP})

	var service corev1.Service
	require.NoError(t, yaml.Unmarshal([]byte(documents[1]), &service))
	require.Contains(t, service.Spec.Ports, corev1.ServicePort{Name: "webhook", Port: 9443, TargetPort: intstr.FromString("webhook"), Protocol: corev1.ProtocolTCP})
}

func TestConfigDefaultsObjectImage(t *testing.T) {
	data, err := fs.ReadFile(Manifests, "config.yaml")
	require.NoError(t, err)

	var config corev1.ConfigMap
	require.NoError(t, yaml.Unmarshal(data, &config))
	require.Equal(t, "ghcr.io/azure/racer-object:"+version.Version, config.Data["RACER_OBJECT_IMAGE"])
	require.Equal(t, ":9443", config.Data["RACER_WEBHOOK_ADDRESS"])
}

func ptr[T any](v T) *T { return &v }
