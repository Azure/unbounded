// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
)

func TestParseCacheSize(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"512Mi", 512 << 20},
		{"536870912", 512 << 20},
		{"536870913", 576 << 20},
		{"576Mi", 576 << 20},
		{"513Mi", 576 << 20},
		{"512.5Mi", 576 << 20},
		{"640M", 640 << 20},
		{"64e7", 640 << 20},
		{"640000000000m", 640 << 20},
		{"10Gi", 10 << 30},
		{"10240Mi", 10 << 30},
		{"10737418240", 10 << 30},
		{"1.5Gi", 1536 << 20},
		{"2Ti", 2 << 40},
		{"3Ti", 3 << 40},
		{"2.5Ti", 5 << 39},
		{"1Pi", 1 << 50},
		{"7Ei", 7 << 60},
		{"9223372036787666943", racer.MaxCacheSizeBytes},
		{"9223372036787666944", racer.MaxCacheSizeBytes},
		{"8589934591.9375Gi", racer.MaxCacheSizeBytes},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := racer.ParseCacheSize(tc.value)
			if err != nil || got != tc.want {
				t.Fatalf("ParseCacheSize(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
			}

			// Quantity decoding may select the arbitrary-precision representation.
			quantity := resource.MustParse(tc.value)
			quantity.ToDec()
			before := quantity.DeepCopy()

			got, err = racer.NormalizeCacheSize(quantity)
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeCacheSize(decimal %q) = %d, %v; want %d", tc.value, got, err, tc.want)
			}

			if !reflect.DeepEqual(quantity, before) {
				t.Fatal("normalization mutated quantity")
			}
		})
	}
}

func TestParseCacheSizeRejectsInvalidRequests(t *testing.T) {
	for _, value := range []string{
		"", "garbage", "10GiB", "10GB", " 10Gi", "10Gi ", "NaN", "Inf",
		"0", "-512Mi", "1", "511Mi", "536870911", "1m", "1e-100", "32Mi",
		"536870912.1", "512.1Mi", "536870912001m", "536870912.000000001",
		"9223372036787666945", "9223372036854775807", "9223372036854775808",
		"8589934592Gi", "8Ei", "100000000000000000000Ti", "1e100",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := racer.ParseCacheSize(value); err == nil || got != 0 {
				t.Fatalf("ParseCacheSize(%q) = %d, %v; want zero and error", value, got, err)
			}
		})
	}
}

func TestCacheSizeRangeErrorUnits(t *testing.T) {
	for _, value := range []string{"511Mi", "9223372036787666945"} {
		t.Run(value, func(t *testing.T) {
			_, err := racer.ParseCacheSize(value)
			if err == nil || err.Error() != "cache size must be between 512Mi and 8589934591.9375Gi" {
				t.Fatalf("ParseCacheSize(%q) error = %v, want range in Kubernetes quantity units", value, err)
			}
		})
	}
}

func TestResolveCacheSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    *corev1.Node
		site    *machinav1alpha3.Site
		want    int64
		wantErr string
	}{
		{name: "nil inputs", want: 10 << 30},
		{name: "empty objects", node: &corev1.Node{}, site: &machinav1alpha3.Site{}, want: 10 << 30},
		{name: "no Site size", site: cacheSite(nil), want: 10 << 30},
		{name: "Site default", site: cacheSite(new(resource.MustParse("2Ti"))), want: 2 << 40},
		{name: "Node only", node: cacheNode("3Ti"), want: 3 << 40},
		{name: "Node wins", node: cacheNode("512.5Mi"), site: cacheSite(new(resource.MustParse("2Ti"))), want: 576 << 20},
		{name: "invalid unused Site", node: cacheNode("3Ti"), site: cacheSite(new(resource.MustParse("0"))), want: 3 << 40},
		{name: "empty override blocks inheritance", node: cacheNode(""), site: cacheSite(new(resource.MustParse("2Ti"))), wantErr: racer.CacheSizeAnnotationKey},
		{name: "invalid override blocks inheritance", node: cacheNode("invalid"), site: cacheSite(new(resource.MustParse("2Ti"))), wantErr: racer.CacheSizeAnnotationKey},
		{name: "invalid override blocks builtin", node: cacheNode("0"), wantErr: racer.CacheSizeAnnotationKey},
		{name: "zero Site blocks builtin", site: cacheSite(new(resource.MustParse("0"))), wantErr: "spec.components.racer.cacheSize"},
		{name: "fractional Site blocks builtin", site: cacheSite(new(resource.MustParse("512.1Mi"))), wantErr: "spec.components.racer.cacheSize"},
		{name: "overflow Site blocks builtin", site: cacheSite(new(resource.MustParse("8Ei"))), wantErr: "spec.components.racer.cacheSize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeBefore, siteBefore := tc.node.DeepCopy(), tc.site.DeepCopy()

			got, err := racer.ResolveCacheSize(tc.node, tc.site)
			if got != tc.want || (err != nil) != (tc.wantErr != "") {
				t.Fatalf("ResolveCacheSize() = %d, %v; want %d, error containing %q", got, err, tc.want, tc.wantErr)
			}

			if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not identify source %q", err, tc.wantErr)
			}

			if !reflect.DeepEqual(tc.node, nodeBefore) || !reflect.DeepEqual(tc.site, siteBefore) {
				t.Fatal("resolution mutated Node or Site")
			}
		})
	}
}

func TestResolveCacheSizeLiveInheritance(t *testing.T) {
	node := &corev1.Node{}
	site := cacheSite(new(resource.MustParse("2Ti")))
	assertSize := func(want int64) {
		t.Helper()

		if got, err := racer.ResolveCacheSize(node, site); err != nil || got != want {
			t.Fatalf("ResolveCacheSize() = %d, %v; want %d", got, err, want)
		}
	}
	assertSize(2 << 40)

	site.Spec.Components.Racer.CacheSize = new(resource.MustParse("3Ti"))

	assertSize(3 << 40)

	if node.Annotations != nil {
		t.Fatal("Site default copied to Node")
	}

	node.Annotations = map[string]string{racer.CacheSizeAnnotationKey: "10Gi"}

	assertSize(10 << 30)

	site.Spec.Components.Racer.CacheSize = new(resource.MustParse("4Ti"))

	assertSize(10 << 30)
	delete(node.Annotations, racer.CacheSizeAnnotationKey)
	assertSize(4 << 40)

	site.Spec.Components.Racer.CacheSize = nil

	assertSize(10 << 30)
}

func cacheNode(value string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", Annotations: map[string]string{racer.CacheSizeAnnotationKey: value},
	}}
}

func cacheSite(size *resource.Quantity) *machinav1alpha3.Site {
	return &machinav1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: machinav1alpha3.SiteSpec{Components: machinav1alpha3.SiteComponents{
			Racer: &machinav1alpha3.RacerComponentSpec{CacheSize: size},
		}},
	}
}
