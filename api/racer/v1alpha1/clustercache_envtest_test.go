// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const testClientID = "00000000-0000-0000-0000-000000000001"

func validOrigin() map[string]any {
	return map[string]any{
		"azure": map[string]any{"containerURL": "https://account.blob.core.windows.net/models"},
	}
}

func azureCredential(credential map[string]any) map[string]any {
	origin := validOrigin()
	origin["azure"].(map[string]any)["credential"] = credential

	return origin
}

func withOrigin(mutate func(map[string]any)) map[string]any {
	origin := validOrigin()
	mutate(origin)

	return origin
}

func TestEnvtestClusterCacheOriginValidation(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API validation tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"crd"}, ErrorIfCRDPathMissing: true}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	c, err := client.New(rc, client.Options{Scheme: scheme})
	require.NoError(t, err)

	n := 0
	create := func(spec map[string]any) error {
		n++

		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "ClusterCache",
			"metadata":   map[string]any{"name": fmt.Sprintf("cache-%d", n)},
		}}
		if spec != nil {
			obj.Object["spec"] = spec
		}

		return c.Create(t.Context(), obj)
	}

	valid := map[string]map[string]any{
		"no spec":    nil,
		"empty spec": {},
		"anonymous":  {"origin": validOrigin()},
		"anonymous http azurite": {"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "http://localhost:10000/devstoreaccount1/models"}
		})},
		"default":           {"origin": azureCredential(map[string]any{"default": map[string]any{}})},
		"default client":    {"origin": azureCredential(map[string]any{"default": map[string]any{"clientID": testClientID}})},
		"default secret":    {"origin": azureCredential(map[string]any{"default": map[string]any{"secretRef": map[string]any{"name": "azure"}}})},
		"workload identity": {"origin": azureCredential(map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID, "tenantID": testClientID}})},
		"tuning": {"origin": withOrigin(func(o map[string]any) {
			o["metadataTTL"] = "0s"
			o["requestTimeout"] = "1m30s"
			o["maxConcurrentRequests"] = int64(65536)
		})},
	}
	for _, url := range []string{
		"https://account.blob.core.windows.net/abc",
		"https://account.blob.core.windows.net/a-b-1",
		"https://account.blob.core.windows.net:443/models",
		"https://account.blob.core.windows.net/" + strings.Repeat("x", 63),
		"https://[::1]/account/models",
	} {
		valid["container url "+url] = map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": url} })}
	}

	for name, spec := range valid {
		t.Run("accepts "+name, func(t *testing.T) {
			require.NoError(t, create(spec))
		})
	}

	invalid := map[string]struct {
		spec    map[string]any
		message string
	}{
		"no provider":         {map[string]any{"origin": map[string]any{}}, "azure is required"},
		"negative ttl":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["metadataTTL"] = "-1s" })}, "metadataTTL must not be negative"},
		"zero timeout":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["requestTimeout"] = "0s" })}, "requestTimeout must be positive"},
		"bad duration":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["requestTimeout"] = "soon" })}, "requestTimeout"},
		"zero concurrency":    {map[string]any{"origin": withOrigin(func(o map[string]any) { o["maxConcurrentRequests"] = int64(0) })}, "maxConcurrentRequests"},
		"large concurrency":   {map[string]any{"origin": withOrigin(func(o map[string]any) { o["maxConcurrentRequests"] = int64(65537) })}, "maxConcurrentRequests"},
		"no container url":    {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{} })}, "containerURL"},
		"empty container url": {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": ""} })}, "containerURL"},
		"not a url":           {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "account"} })}, "containerURL must be an http or https URL"},
		"ftp url":             {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "ftp://account/models"} })}, "containerURL must be an http or https URL"},
		"url query":           {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/models?sig=x"} })}, "must not contain credentials"},
		"url fragment":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/models#x"} })}, "must not contain credentials"},
		"url userinfo":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://user@account/models"} })}, "must not contain credentials"},
		"account only": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://account.blob.core.windows.net"}
		})}, "one valid container name"},
		"trailing slash": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://account.blob.core.windows.net/"}
		})}, "one valid container name"},
		"container trailing slash": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://account.blob.core.windows.net/models/"}
		})}, "one valid container name"},
		"blob path": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://account/devstoreaccount1/models/key"}
		})}, "one valid container name"},
		"uppercase container": {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/Models"} })}, "one valid container name"},
		"short container":     {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/ab"} })}, "one valid container name"},
		"long container": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://account/" + strings.Repeat("x", 64)}
		})}, "one valid container name"},
		"leading hyphen":          {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/-abc"} })}, "one valid container name"},
		"double hyphen":           {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/a--b"} })}, "one valid container name"},
		"dot container":           {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/a.b"} })}, "one valid container name"},
		"escaped container":       {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account/%61bc"} })}, "one valid container name"},
		"variable reference host": {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://$(STORAGE_HOST)/models"} })}, "one valid container name"},
		"underscore host":         {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://acc_ount/models"} })}, "one valid container name"},
		"empty account segment":   {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"containerURL": "https://account//models"} })}, "one valid container name"},
		"url too long": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "https://" + strings.Repeat("a", 310) + "/models"}
		})}, "containerURL"},
		"empty credential": {map[string]any{"origin": azureCredential(map[string]any{})}, "exactly one of default or workloadIdentity is required"},
		"two credentials":  {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{}, "workloadIdentity": map[string]any{"clientID": testClientID}})}, "exactly one of default or workloadIdentity is required"},
		"http default": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "http://account/models", "credential": map[string]any{"default": map[string]any{}}}
		})}, "containerURL must use https"},
		"http workload": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"containerURL": "http://account/models", "credential": map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID}}}
		})}, "containerURL must use https"},
		"client and secret":     {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{"clientID": testClientID, "secretRef": map[string]any{"name": "azure"}}})}, "clientID and secretRef are mutually exclusive"},
		"malformed secret name": {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{"secretRef": map[string]any{"name": "bad/name"}}})}, "name"},
		"empty secret name":     {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{"secretRef": map[string]any{"name": ""}}})}, "name"},
		"bad default client":    {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{"clientID": "not-a-uuid"}})}, "clientID"},
		"workload no client":    {map[string]any{"origin": azureCredential(map[string]any{"workloadIdentity": map[string]any{}})}, "clientID"},
		"workload bad tenant":   {map[string]any{"origin": azureCredential(map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID, "tenantID": "tenant"}})}, "tenantID"},
	}
	for name, tc := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			err := create(tc.spec)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, tc.message)
		})
	}

	t.Run("preserves fields", func(t *testing.T) {
		spec := map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["metadataTTL"] = "10s"
			o["requestTimeout"] = "2m0s"
			o["maxConcurrentRequests"] = int64(32)
			o["azure"] = map[string]any{
				"containerURL": "https://account.blob.core.windows.net/models",
				"credential":   map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID, "tenantID": testClientID}},
			}
		})}
		require.NoError(t, create(spec))

		cache := &ClusterCache{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("cache-%d", n)}, cache))
		origin := cache.Spec.Origin
		require.NotNil(t, origin)
		require.Equal(t, "10s", origin.MetadataTTL.Duration.String())
		require.Equal(t, "2m0s", origin.RequestTimeout.Duration.String())
		require.Equal(t, int32(32), *origin.MaxConcurrentRequests)
		require.Equal(t, "https://account.blob.core.windows.net/models", origin.Azure.ContainerURL)
		require.Equal(t, &AzureWorkloadIdentityCredential{ClientID: testClientID, TenantID: testClientID}, origin.Azure.Credential.WorkloadIdentity)
		require.Nil(t, origin.Azure.Credential.Default)
	})

	update := func(t *testing.T, mutate func(origin map[string]any)) error {
		t.Helper()

		require.NoError(t, create(map[string]any{"origin": validOrigin()}))

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(GroupVersion.WithKind("ClusterCache"))
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("cache-%d", n)}, obj))

		origin, found, err := unstructured.NestedMap(obj.Object, "spec", "origin")
		require.NoError(t, err)
		require.True(t, found)
		mutate(origin)
		require.NoError(t, unstructured.SetNestedMap(obj.Object, origin, "spec", "origin"))

		return c.Update(t.Context(), obj)
	}

	immutable := map[string]struct {
		mutate  func(map[string]any)
		message string
	}{
		"container": {func(o map[string]any) {
			o["azure"].(map[string]any)["containerURL"] = "https://account.blob.core.windows.net/other"
		}, "containerURL is immutable"},
		"account": {func(o map[string]any) {
			o["azure"].(map[string]any)["containerURL"] = "https://other.blob.core.windows.net/models"
		}, "containerURL is immutable"},
		"container url spelling": {func(o map[string]any) {
			o["azure"].(map[string]any)["containerURL"] = "https://account.blob.core.windows.net:443/models"
		}, "containerURL is immutable"},
	}
	for name, tc := range immutable {
		t.Run("rejects update of "+name, func(t *testing.T) {
			err := update(t, tc.mutate)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, tc.message)
		})
	}

	replace := func(t *testing.T, initial map[string]any, mutate func(obj *unstructured.Unstructured)) error {
		t.Helper()

		require.NoError(t, create(initial))

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(GroupVersion.WithKind("ClusterCache"))
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("cache-%d", n)}, obj))
		mutate(obj)

		return c.Update(t.Context(), obj)
	}

	presence := map[string]struct {
		initial map[string]any
		mutate  func(*unstructured.Unstructured)
	}{
		"removal of origin": {map[string]any{"origin": validOrigin()}, func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec", "origin")
		}},
		"removal of spec": {map[string]any{"origin": validOrigin()}, func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec")
		}},
		"addition of origin": {map[string]any{}, func(o *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedMap(o.Object, validOrigin(), "spec", "origin"))
		}},
		"addition of spec with origin": {nil, func(o *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedMap(o.Object, map[string]any{"origin": validOrigin()}, "spec"))
		}},
	}
	for name, tc := range presence {
		t.Run("rejects "+name, func(t *testing.T) {
			err := replace(t, tc.initial, tc.mutate)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "spec.origin cannot be added or removed")
		})
	}

	t.Run("rejects source swap through origin removal", func(t *testing.T) {
		for _, path := range [][]string{{"spec", "origin"}, {"spec"}} {
			require.Error(t, replace(t, map[string]any{"origin": validOrigin()}, func(o *unstructured.Unstructured) {
				unstructured.RemoveNestedField(o.Object, path...)
			}))

			// The removal never landed, so a re-add with another account is an
			// ordinary in-place change and hits the containerURL rule.
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(GroupVersion.WithKind("ClusterCache"))
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("cache-%d", n)}, obj))
			_, found, err := unstructured.NestedMap(obj.Object, "spec", "origin")
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, unstructured.SetNestedField(obj.Object, "https://other.blob.core.windows.net/models", "spec", "origin", "azure", "containerURL"))
			err = c.Update(t.Context(), obj)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "containerURL is immutable")
		}
	})

	t.Run("allows spec without origin to stay without origin", func(t *testing.T) {
		require.NoError(t, replace(t, map[string]any{}, func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec")
		}))
	})

	mutable := map[string]func(map[string]any){
		"metadataTTL":           func(o map[string]any) { o["metadataTTL"] = "5s" },
		"requestTimeout":        func(o map[string]any) { o["requestTimeout"] = "2m0s" },
		"maxConcurrentRequests": func(o map[string]any) { o["maxConcurrentRequests"] = int64(8) },
		"credential": func(o map[string]any) {
			o["azure"].(map[string]any)["credential"] = map[string]any{"default": map[string]any{}}
		},
		"unchanged source": func(o map[string]any) {
			o["azure"].(map[string]any)["containerURL"] = "https://account.blob.core.windows.net/models"
		},
	}
	for name, mutate := range mutable {
		t.Run("allows update of "+name, func(t *testing.T) {
			require.NoError(t, update(t, mutate))
		})
	}
}

func TestEnvtestClusterCachePriorityValidation(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API validation tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"crd"}, ErrorIfCRDPathMissing: true}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	c, err := client.New(rc, client.Options{Scheme: scheme})
	require.NoError(t, err)

	n := 0
	create := func(spec map[string]any) (string, error) {
		n++
		name := fmt.Sprintf("priority-%d", n)
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "ClusterCache",
			"metadata":   map[string]any{"name": name},
			"spec":       spec,
		}}

		return name, c.Create(t.Context(), obj)
	}

	for name, spec := range map[string]map[string]any{
		"unset":   {},
		"minimum": {"priority": int64(0)},
		"default": {"priority": int64(500)},
		"maximum": {"priority": int64(1000)},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			_, err := create(spec)
			require.NoError(t, err)
		})
	}

	for name, priority := range map[string]int64{"negative": -1, "above maximum": 1001} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := create(map[string]any{"priority": priority})
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "spec.priority")
		})
	}

	t.Run("unset stays unset", func(t *testing.T) {
		name, err := create(map[string]any{})
		require.NoError(t, err)

		cache := &ClusterCache{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, cache))
		require.Nil(t, cache.Spec.Priority)
		require.Equal(t, DefaultPriority, cache.Spec.EffectivePriority())
	})

	t.Run("allows priority updates", func(t *testing.T) {
		name, err := create(map[string]any{"priority": int64(100)})
		require.NoError(t, err)

		for _, priority := range []any{int64(900), int64(0), nil} {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(GroupVersion.WithKind("ClusterCache"))
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, obj))

			if priority == nil {
				unstructured.RemoveNestedField(obj.Object, "spec", "priority")
			} else {
				require.NoError(t, unstructured.SetNestedField(obj.Object, priority, "spec", "priority"))
			}

			require.NoError(t, c.Update(t.Context(), obj))
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(GroupVersion.WithKind("ClusterCache"))
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, obj))
		require.NoError(t, unstructured.SetNestedField(obj.Object, int64(1001), "spec", "priority"))
		err = c.Update(t.Context(), obj)
		require.True(t, apierrors.IsInvalid(err), "%v", err)
	})
}
