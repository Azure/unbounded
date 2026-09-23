// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

func TestB06ConfiguredSocketRootValidation(t *testing.T) {
	for _, text := range []string{"", "relative", "9090", "/bad\x00root"} {
		if _, _, err := racer.CacheSockets(text, "cache"); err == nil {
			t.Errorf("accepted invalid socket root %q", text)
		}
	}

	for _, text := range []string{"/dev/racer", "/run/cache", "/run/9443"} {
		cache, origin, err := racer.CacheSockets(text, "cache")
		if err != nil {
			t.Fatal(err)
		}

		if cache != filepath.Join(text, "cache", "cache") || origin != filepath.Join(text, "cache", "origin") {
			t.Fatal("socket paths do not follow deployment root")
		}
	}
}

func TestB12ProductionSnapshots(t *testing.T) {
	dir := os.Getenv("B12_EXPORT")
	if dir == "" {
		dir = t.TempDir()
	}

	n, p, cache := fixtures()

	r := newTestReconciler(fakeKube(n, p, cache))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal(err)
	}

	g := r.loaded["default"]

	idx, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	snap := idx.snapshot(g.Nodes[n.Name].ID)
	response := get(handler(r.server), target(snap), "", "")

	var envelope pb.Configuration
	if response.Code != 200 || proto.Unmarshal(response.Body.Bytes(), &envelope) != nil {
		t.Fatal("snapshot delivery failed")
	}

	v := envelope.GetSnapshot().Volumes[0]
	if v.CacheSocket != "/dev/racer/volume/cache" || v.OriginSocket != "/dev/racer/volume/origin" || g.Volume.Slots != racer.SlotCount {
		t.Fatalf("invalid cache snapshot: %v", v)
	}

	data, err := protojson.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "cache.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapSiteUniverse(t *testing.T) {
	for _, tc := range []struct {
		deprecated, label, pod string
		valid                  bool
	}{
		{"", "default", "default", true},
		{"default", "default", "default", true},
		{"other", "other", "other", true},
		{"other", "default", "default", true},
		{"default", "other", "default", false},
		{"", "", "default", false},
		{"other", "other", "default", false},
		{"default", "", "default", false},
		{"", "default", "", false},
	} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node", Labels: map[string]string{racer.DeprecatedSiteLabelKey: tc.deprecated, racer.SiteLabelKey: tc.label}}}
		if err := validateBootstrapNode(n, tc.pod); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}

		n.UID = ""
		if validateBootstrapNode(n, tc.pod) == nil {
			t.Fatal("missing UID accepted")
		}
	}

	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node", Labels: map[string]string{racer.DeprecatedSiteLabelKey: "default"}}}
	if err := validateBootstrapNode(n, "default"); err != nil {
		t.Fatalf("absent canonical Site must permit fallback: %v", err)
	}

	n.Labels[racer.ExcludeLabelKey] = "true"
	if validateBootstrapNode(n, "default") == nil {
		t.Fatal("excluded fallback Node accepted")
	}

	delete(n.Labels, racer.ExcludeLabelKey)

	n.DeletionTimestamp = new(metav1.Now())
	if validateBootstrapNode(n, "default") == nil {
		t.Fatal("deleting Node accepted")
	}

	if validateBootstrapNode(nil, "default") == nil {
		t.Fatal("nil Node accepted")
	}
}
