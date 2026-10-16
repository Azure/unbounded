// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWebhookInstalledWithExpectedShape(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	require.Equal(t, "mirror.test/unbounded/racer-object:v1", cm.Data["RACER_OBJECT_IMAGE"])

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookName}, webhook))

	marker := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.NoError(t, validateRuntimeOwner(webhook, marker.UID))
	require.Len(t, webhook.Webhooks, 1)

	hook := webhook.Webhooks[0]
	require.Equal(t, []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Create},
		Rule: admissionv1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"pods"},
			Scope:       ptr.To(admissionv1.NamespacedScope),
		},
	}}, hook.Rules)
	require.Equal(t, &metav1.LabelSelector{MatchLabels: map[string]string{"racer.unbounded-cloud.io/object-sidecar": "enabled"}}, hook.ObjectSelector)
	require.Equal(t, &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"custom-system"},
	}}}, hook.NamespaceSelector)
	require.Len(t, hook.MatchConditions, 1)
	require.Contains(t, hook.MatchConditions[0].Expression, "racer-object")
	require.Equal(t, ptr.To(admissionv1.Fail), hook.FailurePolicy)
	require.Equal(t, &admissionv1.ServiceReference{Namespace: "custom-system", Name: controllerName, Path: ptr.To("/mutate-racer-object-sidecar"), Port: ptr.To[int32](9443)}, hook.ClientConfig.Service)

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	require.Equal(t, servingRoots(secret), hook.ClientConfig.CABundle)
}

func TestWebhookCABundleFollowsRotationWithCaches(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	at := stateOf(t, secret).CreatedAt.Add(caRotationInterval)
	old := servingRoots(secret)

	for range 4 {
		plan, _, err := planAt(t.Context(), env, at)
		require.NoError(t, err)
		persist(t, env, plan)
	}

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	require.NotEqual(t, old, servingRoots(secret))
	requireWebhookCABundle(t, env, servingRoots(secret))
}

func TestWebhookDriftRepairedAndForeignRejected(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookName}, webhook))
	webhook.Webhooks[0].ClientConfig.CABundle = []byte("stale")
	require.NoError(t, env.Client.Update(t.Context(), webhook))
	persist(t, env, planPass(t, env))

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	requireWebhookCABundle(t, env, servingRoots(secret))

	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookName}, webhook))
	delete(webhook.Annotations, runtimeInstallationAnnotation)
	require.NoError(t, env.Client.Update(t.Context(), webhook))
	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "refusing adoption")
}

func TestSetWebhookCABundleRejectsMalformed(t *testing.T) {
	require.Error(t, setWebhookCABundle(&unstructured.Unstructured{Object: map[string]any{}}, []byte("x")))
	require.Error(t, setWebhookCABundle(&unstructured.Unstructured{Object: map[string]any{"webhooks": []any{"bad"}}}, []byte("x")))
}
