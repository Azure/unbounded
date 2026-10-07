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
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestEnvtestRenderedNamedRBAC(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server RBAC assertions")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	for _, suffix := range []string{"", "-custom"} {
		t.Run("names"+suffix, func(t *testing.T) {
			ctx := t.Context()
			namespace := "racer-rbac" + suffix
			installation, version := "racer-installation"+suffix, "racer-version"+suffix

			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

			data := map[string]string{"Namespace": namespace}
			if suffix != "" {
				data["InstallationConfigMapName"], data["VersionConfigMapName"] = installation, version
			}

			out := t.TempDir()
			require.NoError(t, render.Render(".", out, data))

			for _, obj := range decodeRenderedObjects(t, out, "rbac.yaml") {
				require.NoError(t, admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("racer-rbac-test")))
			}

			for _, name := range []string{installation, version, "unrelated"} {
				require.NoError(t, admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}))
			}

			controllerConfig := rest.CopyConfig(rc)
			controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":racer-controller"}
			controller, err := kubernetes.NewForConfig(controllerConfig)
			require.NoError(t, err)

			configmaps := controller.CoreV1().ConfigMaps(namespace)

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				_, err := configmaps.Get(ctx, version, metav1.GetOptions{})
				require.NoError(c, err)
			}, 10*time.Second, 100*time.Millisecond)

			for _, name := range []string{installation, version} {
				cm, err := configmaps.Get(ctx, name, metav1.GetOptions{})
				require.NoError(t, err)

				cm.Data = map[string]string{"updated": "true"}
				_, err = configmaps.Update(ctx, cm, metav1.UpdateOptions{})
				require.NoError(t, err)
				_, err = configmaps.Patch(ctx, name, types.MergePatchType, []byte(`{"data":{"patched":"true"}}`), metav1.PatchOptions{})
				require.NoError(t, err)

				options := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()}
				list, err := configmaps.List(ctx, options)
				require.NoError(t, err)
				require.Len(t, list.Items, 1)
				require.Equal(t, name, list.Items[0].Name)

				watch, err := configmaps.Watch(ctx, options)
				require.NoError(t, err)
				watch.Stop()
			}

			_, err = configmaps.Get(ctx, "unrelated", metav1.GetOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)

			for _, selector := range []string{"", "metadata.name=unrelated", "metadata.name!=" + version} {
				options := metav1.ListOptions{FieldSelector: selector}
				_, err = configmaps.List(ctx, options)
				require.True(t, apierrors.IsForbidden(err), "%v", err)

				watch, err := configmaps.Watch(ctx, options)
				if watch != nil {
					watch.Stop()
				}

				require.True(t, apierrors.IsForbidden(err), "%v", err)
			}

			_, err = configmaps.Update(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "unrelated"}}, metav1.UpdateOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			_, err = configmaps.Patch(ctx, "unrelated", types.MergePatchType, []byte(`{"data":{"patched":"true"}}`), metav1.PatchOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			_, err = controller.CoreV1().ConfigMaps("default").Get(ctx, version, metav1.GetOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)

			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{Namespace: namespace, Name: "racer-controller"},
				Client:    controller.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: "controller/pod-uid"},
			}
			record := resourcelock.LeaderElectionRecord{HolderIdentity: lock.Identity(), LeaseDurationSeconds: 15, AcquireTime: metav1.Now(), RenewTime: metav1.Now()}
			_, _, err = lock.Get(ctx)
			require.True(t, apierrors.IsNotFound(err), "%v", err)
			require.NoError(t, lock.Create(ctx, record))
			observed, _, err := lock.Get(ctx)
			require.NoError(t, err)
			require.Equal(t, record.HolderIdentity, observed.HolderIdentity)
			record.LeaderTransitions++
			require.NoError(t, lock.Update(ctx, record))
			observed, _, err = lock.Get(ctx)
			require.NoError(t, err)
			require.Equal(t, record.LeaderTransitions, observed.LeaderTransitions)

			leases := controller.CoordinationV1().Leases(namespace)
			_, err = leases.Get(ctx, "unrelated", metav1.GetOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			lease, err := leases.Get(ctx, "racer-controller", metav1.GetOptions{})
			require.NoError(t, err)

			lease.Name = "unrelated"
			_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			_, err = leases.Patch(ctx, "racer-controller", types.MergePatchType, []byte(`{"metadata":{"labels":{"patched":"true"}}}`), metav1.PatchOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)

			for _, selector := range []string{"", "metadata.name=racer-controller"} {
				options := metav1.ListOptions{FieldSelector: selector}
				_, err = leases.List(ctx, options)
				require.True(t, apierrors.IsForbidden(err), "%v", err)

				watch, err := leases.Watch(ctx, options)
				if watch != nil {
					watch.Stop()
				}

				require.True(t, apierrors.IsForbidden(err), "%v", err)
			}
		})
	}
}
