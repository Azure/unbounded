// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
)

func TestCacheSockets(t *testing.T) {
	cache, origin, err := CacheSockets(SocketRoot, strings.Repeat("a", 63))
	if err != nil || !strings.HasSuffix(cache, "/cache") || !strings.HasSuffix(origin, "/origin") {
		t.Fatalf("paths: %q %q %v", cache, origin, err)
	}

	for _, test := range [][2]string{{"relative", "cache"}, {"/dev/racer", "../cache"}, {"/dev/racer", "a.b"}, {"/" + strings.Repeat("x", 100), "cache"}, {"/dev/\x00racer", "cache"}} {
		if _, _, err := CacheSockets(test[0], test[1]); err == nil {
			t.Fatalf("accepted invalid socket inputs: %q", test)
		}
	}
}

func TestCacheSiteSelection(t *testing.T) {
	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", Labels: map[string]string{"region": "west"}}, Spec: machina.SiteSpec{Components: machina.SiteComponents{Racer: &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: ptr.To(true)}}}}}

	cache := &racerapi.P2PCache{}
	if match, err := CacheSelectsSite(cache, site); err != nil || !match {
		t.Fatalf("empty selector: %t %v", match, err)
	}

	cache.Spec.SiteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "region", Operator: metav1.LabelSelectorOpIn, Values: []string{"west"}}}
	if match, err := CacheSelectsSite(cache, site); err != nil || !match {
		t.Fatalf("expression selector: %t %v", match, err)
	}

	site.Labels["region"] = "east"
	if match, _ := CacheSelectsSite(cache, site); match {
		t.Fatal("label change retained selection")
	}

	cache.Spec.SiteSelector = metav1.LabelSelector{}

	site.Spec.Components.Racer.Enabled = ptr.To(false)
	if match, _ := CacheSelectsSite(cache, site); !match {
		t.Fatal("installation vote must not exclude a live Site")
	}

	cache.Spec.SiteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "region", Operator: "Invalid"}}
	if _, err := CacheSelectsSite(cache, site); err == nil {
		t.Fatal("invalid selector accepted")
	}
}
