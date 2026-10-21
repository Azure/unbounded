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
		"bucket": "models",
		"azure":  map[string]any{"accountURL": "https://account.blob.core.windows.net"},
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
			o["azure"] = map[string]any{"accountURL": "http://localhost:10000/devstoreaccount1"}
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
	for _, bucket := range []string{"a", "b.c", "d_e-f", strings.Repeat("x", 255), "..."} {
		valid["bucket "+bucket] = map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = bucket })}
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
		"no provider":       {map[string]any{"origin": map[string]any{"bucket": "models"}}, "azure is required"},
		"no bucket":         {map[string]any{"origin": withOrigin(func(o map[string]any) { delete(o, "bucket") })}, "bucket"},
		"empty bucket":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = "" })}, "bucket"},
		"bucket list":       {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = []any{"a"} })}, "bucket"},
		"bucket dot":        {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = "." })}, "bucket must not be . or .."},
		"bucket dotdot":     {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = ".." })}, "bucket must not be . or .."},
		"bucket slash":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = "a/b" })}, "bucket"},
		"bucket too long":   {map[string]any{"origin": withOrigin(func(o map[string]any) { o["bucket"] = strings.Repeat("x", 256) })}, "bucket"},
		"negative ttl":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["metadataTTL"] = "-1s" })}, "metadataTTL must not be negative"},
		"zero timeout":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["requestTimeout"] = "0s" })}, "requestTimeout must be positive"},
		"bad duration":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["requestTimeout"] = "soon" })}, "requestTimeout"},
		"zero concurrency":  {map[string]any{"origin": withOrigin(func(o map[string]any) { o["maxConcurrentRequests"] = int64(0) })}, "maxConcurrentRequests"},
		"large concurrency": {map[string]any{"origin": withOrigin(func(o map[string]any) { o["maxConcurrentRequests"] = int64(65537) })}, "maxConcurrentRequests"},
		"no account url":    {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{} })}, "accountURL"},
		"not a url":         {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"accountURL": "account"} })}, "accountURL must be an http or https URL"},
		"ftp url":           {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"accountURL": "ftp://account"} })}, "accountURL must be an http or https URL"},
		"url query":         {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"accountURL": "https://account?sig=x"} })}, "must not contain credentials"},
		"url fragment":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"accountURL": "https://account#x"} })}, "must not contain credentials"},
		"url userinfo":      {map[string]any{"origin": withOrigin(func(o map[string]any) { o["azure"] = map[string]any{"accountURL": "https://user@account"} })}, "must not contain credentials"},
		"url too long": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"accountURL": "https://" + strings.Repeat("a", 250)}
		})}, "accountURL"},
		"empty credential": {map[string]any{"origin": azureCredential(map[string]any{})}, "exactly one of default or workloadIdentity is required"},
		"two credentials":  {map[string]any{"origin": azureCredential(map[string]any{"default": map[string]any{}, "workloadIdentity": map[string]any{"clientID": testClientID}})}, "exactly one of default or workloadIdentity is required"},
		"http default": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"accountURL": "http://account", "credential": map[string]any{"default": map[string]any{}}}
		})}, "accountURL must use https"},
		"http workload": {map[string]any{"origin": withOrigin(func(o map[string]any) {
			o["azure"] = map[string]any{"accountURL": "http://account", "credential": map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID}}}
		})}, "accountURL must use https"},
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
				"accountURL": "https://account.blob.core.windows.net",
				"credential": map[string]any{"workloadIdentity": map[string]any{"clientID": testClientID, "tenantID": testClientID}},
			}
		})}
		require.NoError(t, create(spec))

		cache := &ClusterCache{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("cache-%d", n)}, cache))
		origin := cache.Spec.Origin
		require.NotNil(t, origin)
		require.Equal(t, BucketName("models"), origin.Bucket)
		require.Equal(t, "10s", origin.MetadataTTL.Duration.String())
		require.Equal(t, "2m0s", origin.RequestTimeout.Duration.String())
		require.Equal(t, int32(32), *origin.MaxConcurrentRequests)
		require.Equal(t, "https://account.blob.core.windows.net", origin.Azure.AccountURL)
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
		"bucket": {func(o map[string]any) { o["bucket"] = "other" }, "bucket is immutable"},
		"account url": {func(o map[string]any) {
			o["azure"].(map[string]any)["accountURL"] = "https://other.blob.core.windows.net"
		}, "accountURL is immutable"},
		"account url spelling": {func(o map[string]any) {
			o["azure"].(map[string]any)["accountURL"] = "https://account.blob.core.windows.net/"
		}, "accountURL is immutable"},
	}
	for name, tc := range immutable {
		t.Run("rejects update of "+name, func(t *testing.T) {
			err := update(t, tc.mutate)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, tc.message)
		})
	}

	mutable := map[string]func(map[string]any){
		"metadataTTL":           func(o map[string]any) { o["metadataTTL"] = "5s" },
		"requestTimeout":        func(o map[string]any) { o["requestTimeout"] = "2m0s" },
		"maxConcurrentRequests": func(o map[string]any) { o["maxConcurrentRequests"] = int64(8) },
		"credential": func(o map[string]any) {
			o["azure"].(map[string]any)["credential"] = map[string]any{"default": map[string]any{}}
		},
		"unchanged source": func(o map[string]any) { o["bucket"] = "models" },
	}
	for name, mutate := range mutable {
		t.Run("allows update of "+name, func(t *testing.T) {
			require.NoError(t, update(t, mutate))
		})
	}
}
