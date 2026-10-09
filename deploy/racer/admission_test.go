// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestEnvtestControllerAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API admission and RBAC tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	for _, namespace := range []string{"unbounded-system", "custom-system"} {
		t.Run(namespace, func(t *testing.T) {
			ctx := t.Context()
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
			env := &component.Env{Namespace: namespace}
			objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"node-restriction.yaml", "rbac.yaml"}, nil)
			require.NoError(t, err)

			for _, obj := range objects {
				require.NoError(t, admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("racer-admission-test")))
			}

			config := rest.CopyConfig(rc)
			config.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":racer-controller"}
			controller, err := client.New(config, client.Options{Scheme: scheme.Scheme})
			require.NoError(t, err)

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: namespace + "-worker"}}
			require.NoError(t, admin.Create(ctx, node))

			patch := func(body string) error {
				return controller.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(body)), client.DryRunAll)
			}
			cm := func(name string) *corev1.ConfigMap {
				return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			}
			secret := func(name string) *corev1.Secret {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Type: corev1.SecretTypeOpaque}
			}
			credentials := secret("racer-credentials")
			require.NoError(t, admin.Create(ctx, credentials))
			// Both verdicts must work: fail-closed CEL errors cannot count as success.
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.NoError(c, controller.Update(ctx, credentials.DeepCopy(), client.DryRunAll))
				require.NoError(c, patch(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}}}`))

				annotated := credentials.DeepCopy()
				annotated.Annotations = map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}
				require.NoError(c, controller.Update(ctx, annotated, client.DryRunAll))
				require.ErrorContains(c, patch(`{"spec":{"unschedulable":true}}`), "racer-node-write-restriction")
			}, 15*time.Second, 100*time.Millisecond)

			for _, name := range []string{"racer-installation", "racer-version", "other"} {
				require.True(t, apierrors.IsForbidden(controller.Create(ctx, cm(name), client.DryRunAll)))
			}

			for _, name := range []string{"other", "racer-controller-tls", "racer-config"} {
				err := controller.Create(ctx, secret(name), client.DryRunAll)
				require.True(t, apierrors.IsForbidden(err))
			}

			for _, key := range []string{corev1.ServiceAccountNameKey, corev1.ServiceAccountUIDKey} {
				obj := credentials.DeepCopy()
				obj.Annotations = map[string]string{key: ""}
				// Annotations cannot turn an Opaque Secret into a token Secret.
				require.NoError(t, controller.Update(ctx, obj, client.DryRunAll))
			}

			obj := credentials.DeepCopy()
			obj.Type = corev1.SecretTypeServiceAccountToken
			obj.Annotations = map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}
			err = controller.Update(ctx, obj, client.DryRunAll)
			require.True(t, apierrors.IsInvalid(err))
			require.ErrorContains(t, err, "field is immutable")

			for _, body := range []string{
				`{"metadata":{"labels":{"other":"value"}}}`,
				`{"metadata":{"annotations":{"racer.unbounded-cloud.io/shares":"1"}}}`,
				`{"metadata":{"annotations":{"other":"value"}}}`,
				`{"metadata":{"finalizers":["example.com/hold"]}}`,
				`{"spec":{"taints":[{"key":"other","effect":"NoSchedule"}]}}`,
			} {
				require.ErrorContains(t, patch(body), "racer-node-write-restriction")
			}

			for _, key := range []string{"enrolled-shares", "enrolled-rdma-nics", "last-admitted-member"} {
				require.NoError(t, patch(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/`+key+`":"value"}}}`))
			}

			testNodeFieldRestriction(t, admin, controller, node, namespace)

			// Controller RBAC cannot mutate or create external service accounts.
			err = controller.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: namespace}}, client.DryRunAll)
			require.True(t, apierrors.IsForbidden(err))
		})
	}
}

func testNodeFieldRestriction(t *testing.T, admin, controller client.Client, node *corev1.Node, namespace string) {
	t.Helper()

	ctx := t.Context()
	key := client.ObjectKeyFromObject(node)
	require.NoError(t, admin.Get(ctx, key, node))
	node.Labels = map[string]string{"existing": "keep"}
	node.Annotations = map[string]string{"other": "keep"}
	node.Finalizers = []string{"example.com/hold"}
	node.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "owner", UID: "owner-uid"}}
	node.Spec.Unschedulable = true
	node.Spec.Taints = []corev1.Taint{{Key: "existing", Effect: corev1.TaintEffectNoSchedule}}
	require.NoError(t, admin.Update(ctx, node))

	for _, tc := range []struct {
		name string
		body string
	}{
		{"uncordon", `{"spec":{"unschedulable":false}}`},
		{"remove-taints", `{"spec":{"taints":null}}`},
		{"provider-id", `{"spec":{"providerID":"example://other"}}`},
		{"pod-cidr", `{"spec":{"podCIDR":"10.0.0.0/24","podCIDRs":["10.0.0.0/24"]}}`},
		{"add-label", `{"metadata":{"labels":{"new":"value"}}}`},
		{"change-label", `{"metadata":{"labels":{"existing":"changed"}}}`},
		{"remove-label", `{"metadata":{"labels":{"existing":null}}}`},
		{"remove-labels", `{"metadata":{"labels":null}}`},
		{"add-finalizer", `{"metadata":{"finalizers":["example.com/hold","example.com/new"]}}`},
		{"change-finalizer", `{"metadata":{"finalizers":["example.com/changed"]}}`},
		{"remove-finalizers", `{"metadata":{"finalizers":null}}`},
		{"add-owner", `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"Node","name":"owner","uid":"owner-uid"},{"apiVersion":"v1","kind":"Node","name":"other","uid":"other-uid"}]}}`},
		{"change-owner", `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"Node","name":"other","uid":"other-uid"}]}}`},
		{"remove-owners", `{"metadata":{"ownerReferences":null}}`},
		{"generate-name", `{"metadata":{"generateName":"other-"}}`},
		{"change-other-annotation", `{"metadata":{"annotations":{"other":"changed"}}}`},
		{"remove-other-annotation", `{"metadata":{"annotations":{"other":null}}}`},
		{"remove-annotations", `{"metadata":{"annotations":null}}`},
		{"mixed-spec", `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}},"spec":{"unschedulable":false}}`},
		{"mixed-metadata", `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"},"labels":{"existing":null}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch := client.RawPatch(types.MergePatchType, []byte(tc.body))
			// Prove this is a valid mutation, then require an admission denial, not RBAC.
			require.NoError(t, admin.Patch(ctx, node.DeepCopy(), patch, client.DryRunAll))
			err := controller.Patch(ctx, node.DeepCopy(), patch, client.DryRunAll)
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			require.ErrorContains(t, err, "racer-node-write-restriction")
			require.ErrorContains(t, err, "Racer may")
		})
	}

	// A kubelet status write must not block later controller annotation writes.
	node.Status.Phase = corev1.NodeRunning
	require.NoError(t, admin.Status().Update(ctx, node))
	before := node.DeepCopy()

	for _, suffix := range []string{"enrolled-shares", "enrolled-rdma-nics", "last-admitted-member"} {
		annotation := "racer.unbounded-cloud.io/" + suffix
		for _, value := range []string{`"first"`, `"changed"`, `null`} {
			patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+annotation+`":`+value+`}}}`))
			require.NoError(t, controller.Patch(ctx, node.DeepCopy(), patch, client.FieldOwner("racer-admission-test")))
			require.NoError(t, admin.Get(ctx, key, node))

			if value == `null` {
				require.NotContains(t, node.Annotations, annotation)
			} else {
				require.Equal(t, value[1:len(value)-1], node.Annotations[annotation])
			}
		}
	}

	// Main-resource status payloads are discarded by the API server.
	require.NoError(t, controller.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Terminated"}}`))))
	require.NoError(t, controller.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}},"status":{"phase":"Terminated"}}`))))
	require.NoError(t, admin.Get(ctx, key, node))
	require.Equal(t, "1", node.Annotations["racer.unbounded-cloud.io/enrolled-shares"])
	require.Equal(t, before.Status, node.Status)
	require.NoError(t, controller.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":null}}}`))))
	require.NoError(t, admin.Get(ctx, key, node))
	after := node.DeepCopy()
	after.ResourceVersion = before.ResourceVersion
	after.ManagedFields = before.ManagedFields
	require.Equal(t, before, after)

	statusPatch := client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Terminated"}}`))
	err := controller.Status().Patch(ctx, node.DeepCopy(), statusPatch, client.DryRunAll)
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	require.NotContains(t, err.Error(), "racer-node-write-restriction")

	// Grant status access only in this test to exercise admission behind RBAC.
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-status-test"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes/status"}, Verbs: []string{"patch"}}},
	}
	require.NoError(t, admin.Create(ctx, role))
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "racer-controller", Namespace: namespace}},
	}
	require.NoError(t, admin.Create(ctx, binding))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		err := controller.Status().Patch(ctx, node.DeepCopy(), statusPatch, client.DryRunAll)
		require.True(c, apierrors.IsForbidden(err), "%v", err)
		require.ErrorContains(c, err, "Racer may not change Node fields outside metadata")
	}, 15*time.Second, 100*time.Millisecond)
	require.NoError(t, controller.Status().Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}}}`)), client.DryRunAll))
	err = controller.Status().Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"other":"value"}}}`)), client.DryRunAll)
	require.ErrorContains(t, err, "Racer may not change other Node metadata")
	err = controller.Status().Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}},"status":{"phase":"Terminated"}}`)), client.DryRunAll)
	require.ErrorContains(t, err, "Racer may not change Node fields outside metadata")
	require.NoError(t, admin.Delete(ctx, binding))
	require.NoError(t, admin.Delete(ctx, role))
}
