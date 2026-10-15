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

func TestCatalogSkipsNonCacheVolumesBeforeValidation(t *testing.T) {
	valid := racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: nodeID}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}

	for _, volumeType := range []racerv1.ClusterVolumeType{"", "Future", "cache"} {
		t.Run(string(volumeType), func(t *testing.T) {
			invalid := racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "../invalid", UID: "invalid"}, Spec: racerv1.ClusterVolumeSpec{Type: volumeType}}
			duplicate := valid
			duplicate.Spec.Type = volumeType
			catalog, err := BuildVolumeCatalog([]racerv1.ClusterVolume{invalid, duplicate, valid, duplicate})
			require.NoError(t, err)
			require.Len(t, catalog, 1)
			require.Equal(t, wire.CacheID(nodeID), catalog[0].ID)
			catalog, err = BuildVolumeCatalog([]racerv1.ClusterVolume{invalid, duplicate, {}})
			require.NoError(t, err)
			require.NotNil(t, catalog)
			require.Empty(t, catalog)
		})
	}
}

func TestReadCatalogCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, racerv1.AddToScheme(scheme))

	for _, mode := range []string{"both", "cache only", "volume only", "duplicate name", "duplicate uid", "invalid volume"} {
		t.Run(mode, func(t *testing.T) {
			cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "legacy", UID: nodeID}}
			volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume", UID: otherID}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
			objects := []client.Object{cache, volume}
			want := 2

			switch mode {
			case "cache only":
				objects, want = objects[:1], 1
			case "volume only":
				objects, want = objects[1:], 1
			case "duplicate name":
				volume.Name = cache.Name
			case "duplicate uid":
				volume.UID = cache.UID
			case "invalid volume":
				volume.UID = "invalid"
			}

			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			catalog, err := ReadCatalog(t.Context(), reader)
			if mode == "duplicate name" || mode == "duplicate uid" || mode == "invalid volume" {
				require.ErrorIs(t, err, wire.InvalidRequest)

				var unread CatalogReadError
				require.False(t, errors.As(err, &unread))
				require.Nil(t, catalog)

				return
			}

			require.NoError(t, err)
			require.Len(t, catalog, want)
		})
	}
}

type catalogErrorReader struct {
	client.Reader
	volume bool
	err    error
}

func (r catalogErrorReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_, volume := list.(*racerv1.ClusterVolumeList)
	if volume == r.volume {
		return r.err
	}

	return r.Reader.List(ctx, list, opts...)
}

func TestReadCatalogMissingKindAndFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, racerv1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()

	resource := schema.GroupResource{Group: racerv1.GroupName, Resource: "catalog"}
	for _, volume := range []bool{false, true} {
		for _, err := range []error{
			&meta.NoKindMatchError{GroupKind: racerv1.GroupVersion.WithKind("Absent").GroupKind()},
			apierrors.NewNotFound(resource, ""),
			apierrors.NewForbidden(resource, "", errors.New("denied")),
			errors.New("read failed"),
		} {
			catalog, got := ReadCatalog(t.Context(), catalogErrorReader{Reader: reader, volume: volume, err: err})
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
}
