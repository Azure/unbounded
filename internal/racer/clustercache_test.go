// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
)

func TestCacheSockets(t *testing.T) {
	client, origin, err := CacheSockets(SocketRoot, strings.Repeat("a", 63))
	if err != nil || !strings.HasSuffix(client, "/client/socket") || !strings.HasSuffix(origin, "/origin/socket") {
		t.Fatalf("paths: %q %q %v", client, origin, err)
	}

	for _, name := range []string{"0", "a--9", "01234567-89ab-cdef-0123-456789abcdef", strings.Repeat("a", 63)} {
		for _, root := range []string{SocketRoot, "/custom//racer/../sockets/.", "/"} {
			client, origin, err := CacheSockets(root, name)

			cleanRoot := map[string]string{SocketRoot: "/run/racer", "/custom//racer/../sockets/.": "/custom/sockets", "/": ""}[root]
			if err != nil || client != cleanRoot+"/"+name+"/client/socket" || origin != cleanRoot+"/"+name+"/origin/socket" {
				t.Fatalf("root %q name %q: %q %q %v", root, name, client, origin, err)
			}
		}
	}

	for _, test := range [][2]string{{"", "a"}, {"relative", "a"}, {SocketRoot, ""}, {SocketRoot, ".."}, {SocketRoot, "../a"}, {SocketRoot, "a/b"}, {SocketRoot, "a.b"}, {SocketRoot, "a_b"}, {SocketRoot, "A"}, {SocketRoot, "-a"}, {SocketRoot, "a-"}, {SocketRoot, "a\x00b"}, {SocketRoot, "é"}, {SocketRoot, strings.Repeat("a", 64)}, {"/" + strings.Repeat("x", 100), "a"}, {"/run/\x00racer", "a"}} {
		if _, _, err := CacheSockets(test[0], test[1]); err == nil {
			t.Fatalf("accepted invalid socket inputs: %q", test)
		}
	}

	// Both paths reach the sockaddr_un boundary, including its NUL.
	root := "/" + strings.Repeat("x", 90)
	if _, origin, err := CacheSockets(root, "a"); err != nil || len(origin) != 107 {
		t.Fatalf("107-byte origin rejected: %q %v", origin, err)
	}

	if _, _, err := CacheSockets(root+"x", "a"); err == nil {
		t.Fatal("108-byte origin accepted")
	}
}

func TestCacheSocketsResourceIdentity(t *testing.T) {
	resource := &racerapi.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "same-name", UID: types.UID("old-uid"), Generation: 1}}

	client, origin, err := CacheSockets(SocketRoot, resource.Name)
	if err != nil {
		t.Fatal(err)
	}

	resource.Generation++
	resource.Spec.CacheGeneration++

	stableClient, stableOrigin, err := CacheSockets(SocketRoot, resource.Name)
	if err != nil || stableClient != client || stableOrigin != origin {
		t.Fatalf("generation changed paths: %q %q %v", stableClient, stableOrigin, err)
	}

	resource.UID = types.UID("new-uid")

	newClient, newOrigin, err := CacheSockets(SocketRoot, resource.Name)
	if err != nil || newClient != client || newOrigin != origin {
		t.Fatalf("same-name recreation changed paths: %q %q %v", newClient, newOrigin, err)
	}
}

func TestCacheSiteSelection(t *testing.T) {
	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", Labels: map[string]string{"region": "west"}}}

	cache := &racerapi.ClusterCache{}
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

	if match, _ := CacheSelectsSite(cache, site); !match {
		t.Fatal("a live Site needs no component configuration for cache selection")
	}

	site.DeletionTimestamp = new(metav1.Now())
	if match, _ := CacheSelectsSite(cache, site); match {
		t.Fatal("deleting Site retained selection")
	}

	site.DeletionTimestamp = nil

	cache.DeletionTimestamp = new(metav1.Now())
	if match, _ := CacheSelectsSite(cache, site); match {
		t.Fatal("deleting cache retained selection")
	}

	cache.Spec.SiteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "region", Operator: "Invalid"}}
	if _, err := CacheSelectsSite(cache, site); err == nil {
		t.Fatal("invalid selector accepted")
	}
}
