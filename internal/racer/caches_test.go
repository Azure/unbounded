// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func catalogCache(name string, uid types.UID) racerv1.ClusterCache {
	return racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
}

func TestCanonicalSocketPaths(t *testing.T) {
	for _, name := range []string{"a", "cache-a", "cache.a", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		client, origin, err := CanonicalSocketPaths(name)
		if err != nil || client != "/run/racer/"+name+"/client/socket" || origin != "/run/racer/"+name+"/origin/socket" || len(client) > 107 || len(origin) > 107 {
			t.Fatalf("name %q: %q, %q, %v", name, client, origin, err)
		}
	}

	for _, name := range []string{"", ".", "..", "../cache", "cache/child", "Cache", "cache_a", "a..b", "-a", "a-", "a.-b", "a.b-", "a\x00", "caché", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		client, origin, err := CanonicalSocketPaths(name)
		if !errors.Is(err, wire.InvalidRequest) || client != "" || origin != "" {
			t.Fatalf("invalid name %q: %q, %q, %v", name, client, origin, err)
		}
	}
}

func TestBuildCatalog(t *testing.T) {
	caches := []racerv1.ClusterCache{catalogCache("cache-b", testOtherUID), catalogCache("cache-a", testNodeUID), catalogCache("cache-c", testDaemonSetUID)}

	original := make([]racerv1.ClusterCache, len(caches))
	for i := range caches {
		original[i] = *caches[i].DeepCopy()
	}

	got, err := BuildCatalog(caches)

	want := []wire.CacheDefinition{
		{ID: testNodeUID, Name: "cache-a", ClientSocket: "/run/racer/cache-a/client/socket", OriginSocket: "/run/racer/cache-a/origin/socket"},
		{ID: testOtherUID, Name: "cache-b", ClientSocket: "/run/racer/cache-b/client/socket", OriginSocket: "/run/racer/cache-b/origin/socket"},
		{ID: wire.CacheID(testDaemonSetUID), Name: "cache-c", ClientSocket: "/run/racer/cache-c/client/socket", OriginSocket: "/run/racer/cache-c/origin/socket"},
	}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(caches, original) {
		t.Fatalf("catalog: %#v, %v; inputs: %#v", got, err, caches)
	}

	slices.Reverse(caches)

	again, err := BuildCatalog(caches)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("order changed catalog: %#v, %v", again, err)
	}

	empty, err := BuildCatalog(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty catalog: %#v, %v", empty, err)
	}
	// A terminating object still exists; removal follows its absence from inputs.
	cache := catalogCache("cache-a", testNodeUID)
	cache.DeletionTimestamp = &metav1.Time{}

	got, err = BuildCatalog([]racerv1.ClusterCache{cache})
	if err != nil || len(got) != 1 {
		t.Fatalf("terminating cache: %#v, %v", got, err)
	}

	cache.UID = testOtherUID

	recreated, err := BuildCatalog([]racerv1.ClusterCache{cache})
	if err != nil || recreated[0].ID == got[0].ID || recreated[0].ClientSocket != got[0].ClientSocket {
		t.Fatalf("recreation: %#v, %v", recreated, err)
	}
}

func TestBuildCatalogRejectsWholeInvalidCandidate(t *testing.T) {
	valid := catalogCache("cache-a", testNodeUID)
	for name, invalid := range map[string]racerv1.ClusterCache{
		"missing uid":    catalogCache("cache-b", ""),
		"invalid uid":    catalogCache("cache-b", "invalid"),
		"uppercase uid":  catalogCache("cache-b", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"),
		"duplicate uid":  catalogCache("cache-b", testNodeUID),
		"duplicate name": catalogCache("cache-a", testOtherUID),
		"unsafe name":    catalogCache("../cache", testOtherUID),
		"long path":      catalogCache(strings.Repeat("a", 63)+"."+strings.Repeat("b", 19), testOtherUID),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BuildCatalog([]racerv1.ClusterCache{valid, invalid})
			if !errors.Is(err, wire.InvalidRequest) || got != nil {
				t.Fatalf("partial catalog escaped: %#v, %v", got, err)
			}
		})
	}
}
