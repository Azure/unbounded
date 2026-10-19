// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReadCatalog(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, racerv1.AddToScheme(scheme))

	for _, mode := range []string{"none", "one", "two", "duplicate uid", "invalid uid"} {
		t.Run(mode, func(t *testing.T) {
			first := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "first", UID: nodeID}}
			second := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "second", UID: otherID}}
			objects := []client.Object{first, second}
			want := 2

			switch mode {
			case "none":
				objects, want = nil, 0
			case "one":
				objects, want = objects[:1], 1
			case "duplicate uid":
				second.UID = first.UID
			case "invalid uid":
				second.UID = "invalid"
			}

			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			catalog, err := ReadCatalog(t.Context(), reader)
			if mode == "duplicate uid" || mode == "invalid uid" {
				require.ErrorIs(t, err, wire.InvalidRequest)

				var unread CatalogReadError
				require.False(t, errors.As(err, &unread))
				require.Nil(t, catalog)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, catalog)
			require.Len(t, catalog, want)
		})
	}
}

type catalogErrorReader struct {
	client.Reader
	err error
}

func (r catalogErrorReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*racerv1.ClusterCacheList); ok {
		return r.err
	}

	return r.Reader.List(ctx, list, opts...)
}

func TestReadCatalogMissingKindAndFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, racerv1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()

	resource := schema.GroupResource{Group: racerv1.GroupName, Resource: "clustercaches"}
	for _, err := range []error{
		&meta.NoKindMatchError{GroupKind: racerv1.GroupVersion.WithKind("ClusterCache").GroupKind()},
		apierrors.NewNotFound(resource, ""),
		apierrors.NewForbidden(resource, "", errors.New("denied")),
		errors.New("read failed"),
	} {
		catalog, got := ReadCatalog(t.Context(), catalogErrorReader{Reader: reader, err: err})
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			require.NoError(t, got)
			require.Empty(t, catalog)
		} else {
			require.ErrorIs(t, got, err)

			var unread CatalogReadError
			require.True(t, errors.As(got, &unread))
			require.Nil(t, catalog)
		}
	}
}
