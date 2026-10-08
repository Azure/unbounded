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
			objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml", "node-restriction.yaml", "rbac.yaml"}, nil)
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
			// Both verdicts must work: fail-closed CEL errors cannot count as success.
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.NoError(c, controller.Create(ctx, cm("racer-installation"), client.DryRunAll))
				require.NoError(c, controller.Create(ctx, secret("racer-credentials"), client.DryRunAll))
				require.NoError(c, patch(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}}}`))
				require.ErrorContains(c, controller.Create(ctx, cm("other"), client.DryRunAll), "racer-runtime-write-restriction")
				require.ErrorContains(c, patch(`{"spec":{"unschedulable":true}}`), "racer-node-write-restriction")
			}, 15*time.Second, 100*time.Millisecond)

			for _, name := range []string{"racer-installation", "racer-version"} {
				require.NoError(t, controller.Create(ctx, cm(name), client.DryRunAll))
			}

			for _, name := range []string{"other", "racer-controller-tls", "racer-config"} {
				err := controller.Create(ctx, secret(name), client.DryRunAll)
				require.True(t, apierrors.IsForbidden(err))
				require.ErrorContains(t, err, "racer-runtime-write-restriction")
			}

			for _, key := range []string{corev1.ServiceAccountNameKey, corev1.ServiceAccountUIDKey} {
				obj := secret("racer-credentials")
				obj.Annotations = map[string]string{key: ""}
				require.ErrorContains(t, controller.Create(ctx, obj, client.DryRunAll), "racer-runtime-write-restriction")
			}

			obj := secret("racer-credentials")
			obj.Type = corev1.SecretTypeServiceAccountToken
			obj.Annotations = map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}
			require.ErrorContains(t, controller.Create(ctx, obj, client.DryRunAll), "racer-runtime-write-restriction")

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
			// Controller RBAC cannot mutate or create external service accounts.
			err = controller.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: namespace}}, client.DryRunAll)
			require.True(t, apierrors.IsForbidden(err))
		})
	}
}
