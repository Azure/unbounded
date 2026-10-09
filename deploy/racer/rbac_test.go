// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/authority"
)

func TestEnvtestRuntimeRBACWithoutAdmissionPolicy(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API RBAC tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../api/racer/v1alpha1/crd"}, ErrorIfCRDPathMissing: true}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	require.NoError(t, racerv1.AddToScheme(scheme.Scheme))
	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	ctx := t.Context()
	policies := &admissionv1.ValidatingAdmissionPolicyList{}
	require.NoError(t, admin.List(ctx, policies))
	require.Empty(t, policies.Items)

	bindings := &admissionv1.ValidatingAdmissionPolicyBindingList{}
	require.NoError(t, admin.List(ctx, bindings))
	require.Empty(t, bindings.Items)

	const namespace = "unbounded-system"
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	env := &component.Env{Namespace: namespace}
	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"rbac.yaml"}, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		require.NoError(t, admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("racer-rbac-test")))
	}

	config := rest.CopyConfig(rc)
	config.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":racer-controller", Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + namespace}}
	controller, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	for _, kind := range []string{"ConfigMap", "Secret"} {
		for _, name := range []string{"racer-installation", "racer-version", "racer-credentials", "racer-controller-tls", "racer-config", "unrelated"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				meta := metav1.ObjectMeta{Name: name, Namespace: namespace}

				var obj client.Object = &corev1.ConfigMap{ObjectMeta: meta}
				if kind == "Secret" {
					obj = &corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque}
				}

				require.True(t, apierrors.IsForbidden(controller.Create(ctx, obj)))

				if kind == "Secret" {
					token := obj.DeepCopyObject().(*corev1.Secret)
					token.Type = corev1.SecretTypeServiceAccountToken
					token.Annotations = map[string]string{corev1.ServiceAccountNameKey: "racer-controller"}
					require.True(t, apierrors.IsForbidden(controller.Create(ctx, token)))
				}

				err := controller.Update(ctx, obj.DeepCopyObject().(client.Object))
				require.True(t, apierrors.IsForbidden(err) || apierrors.IsNotFound(err), "%v", err)

				apply := component.ToUnstructured(obj)
				apply.SetAPIVersion("v1")
				apply.SetKind(kind)
				require.True(t, apierrors.IsForbidden(controller.Apply(ctx, client.ApplyConfigurationFromUnstructured(apply), client.FieldOwner("runtime-test"))))
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(obj), obj)))

				named := kind == "ConfigMap" && (name == "racer-installation" || name == "racer-version") || kind == "Secret" && name == "racer-credentials"
				if !named {
					require.NoError(t, admin.Create(ctx, obj))
					obj.SetLabels(map[string]string{"changed": "true"})
					require.True(t, apierrors.IsForbidden(controller.Update(ctx, obj)))
					require.True(t, apierrors.IsForbidden(controller.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"changed":"true"}}}`)))))
					require.True(t, apierrors.IsForbidden(controller.Apply(ctx, client.ApplyConfigurationFromUnstructured(apply), client.FieldOwner("runtime-test"), client.ForceOwnership)))
					require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(obj), obj))
					require.Empty(t, obj.GetLabels())
				}
			})
		}
	}

	cfg := authority.Config{
		Cluster: "00000000-0000-4000-8000-000000000001", Namespace: namespace,
		DataplaneServiceAccount: "racer-dataplane", ControllerServiceAccount: "racer-controller", DaemonSetName: "racer-dataplane",
		CredentialsSecretName: "racer-credentials", VersionConfigMapName: "racer-version", InstallationConfigMapName: "racer-installation",
		Rotation: authority.RotationPolicy{Interval: time.Hour, PrepareFor: time.Minute, RetainFor: 24 * time.Hour},
	}
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cfg.InstallationConfigMapName, Namespace: namespace}, Data: map[string]string{
		"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", "initialization_protocol": "staged-v1",
	}}
	require.NoError(t, admin.Create(ctx, marker))

	now := time.Now().UTC().Truncate(time.Second)
	initializer := authority.New(cfg, authority.Dependencies{Reader: admin, Writer: admin, Now: func() time.Time { return now }})
	require.NoError(t, initializer.Recover(ctx, admin))
	_, err = initializer.ReconcileCredentials(ctx)
	require.NoError(t, err)

	runtime := authority.New(cfg, authority.Dependencies{Reader: controller, Writer: controller, Now: func() time.Time { return now }})
	require.NoError(t, runtime.Recover(ctx, controller))
	require.NoError(t, runtime.Observe(ctx))

	for _, name := range []string{cfg.InstallationConfigMapName, cfg.VersionConfigMapName} {
		cm := &corev1.ConfigMap{}
		require.NoError(t, controller.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm))
		cm.Labels = map[string]string{"runtime-update": "allowed"}
		require.NoError(t, controller.Update(ctx, cm))
		require.NoError(t, controller.Patch(ctx, cm, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"runtime-patch":"allowed"}}}`))))
	}

	before := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: cfg.CredentialsSecretName}
	require.NoError(t, controller.Get(ctx, key, before))
	require.Equal(t, corev1.SecretTypeOpaque, before.Type)

	for _, method := range []string{"update", "patch", "apply"} {
		t.Run("token-conversion/"+method, func(t *testing.T) {
			obj := before.DeepCopy()
			obj.Type = corev1.SecretTypeServiceAccountToken
			obj.Annotations[corev1.ServiceAccountNameKey] = "racer-controller"

			var err error

			switch method {
			case "update":
				err = controller.Update(ctx, obj)
			case "patch":
				err = controller.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(`{"type":"kubernetes.io/service-account-token","metadata":{"annotations":{"kubernetes.io/service-account.name":"racer-controller"}}}`)))
			case "apply":
				apply := component.ToUnstructured(obj)
				apply.SetAPIVersion("v1")
				apply.SetKind("Secret")
				apply.SetManagedFields(nil)
				err = controller.Apply(ctx, client.ApplyConfigurationFromUnstructured(apply), client.FieldOwner("runtime-test"), client.ForceOwnership)
			}

			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "field is immutable")
		})
	}

	now = now.Add(cfg.Rotation.Interval)
	_, err = runtime.ReconcileCredentials(ctx)
	require.NoError(t, err)

	after := &corev1.Secret{}
	require.NoError(t, controller.Get(ctx, key, after))
	require.Equal(t, before.UID, after.UID)
	require.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
	require.NotEqual(t, before.Data["bundle.json"], after.Data["bundle.json"])

	now = now.Add(cfg.Rotation.PrepareFor)
	_, err = runtime.ReconcileCredentials(ctx)
	require.NoError(t, err)
	require.NoError(t, runtime.TrustReady())
}
