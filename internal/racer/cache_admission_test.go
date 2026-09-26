// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

// Run against the generated CRD in TestEnvtestServer, including Kubernetes'
// built-in metadata validation rather than a fake client or a CEL-only evaluator.
func integrationCacheNameAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, tt := range []struct {
		name        string
		cacheName   string
		wantMessage string
	}{
		{name: "single-character", cacheName: "a"},
		{name: "digits-and-hyphens", cacheName: "0.cache-1.2"},
		{name: "63-character-label", cacheName: strings.Repeat("a", 63)},
		{name: "64-total-multiple-labels", cacheName: strings.Repeat("a", 62) + ".b"},
		{name: "82-total-first-label-boundary", cacheName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)},
		{name: "82-total-last-label-boundary", cacheName: strings.Repeat("a", 18) + "." + strings.Repeat("b", 63)},
		{name: "82-total-many-labels", cacheName: strings.Repeat("a.", 40) + "bb"},
		{name: "64-character-label", cacheName: strings.Repeat("a", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-first-label", cacheName: strings.Repeat("a", 64) + ".b", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-middle-label", cacheName: "a." + strings.Repeat("b", 64) + ".c", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-last-label", cacheName: "a." + strings.Repeat("b", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "83-total-valid-labels", cacheName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 19), wantMessage: "name must fit the canonical Unix socket path"},
		{name: "empty", cacheName: "", wantMessage: "metadata.name"},
		{name: "uppercase", cacheName: "Cache", wantMessage: "metadata.name"},
		{name: "underscore", cacheName: "cache_a", wantMessage: "metadata.name"},
		{name: "non-ASCII", cacheName: "caché", wantMessage: "metadata.name"},
		{name: "slash", cacheName: "cache/child", wantMessage: "metadata.name"},
		{name: "leading-dot", cacheName: ".cache", wantMessage: "metadata.name"},
		{name: "trailing-dot", cacheName: "cache.", wantMessage: "metadata.name"},
		{name: "empty-label", cacheName: "cache..a", wantMessage: "metadata.name"},
		{name: "leading-hyphen", cacheName: "-cache", wantMessage: "metadata.name"},
		{name: "trailing-hyphen", cacheName: "cache-", wantMessage: "metadata.name"},
		{name: "label-leading-hyphen", cacheName: "cache.-a", wantMessage: "metadata.name"},
		{name: "label-trailing-hyphen", cacheName: "cache.a-", wantMessage: "metadata.name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: tt.cacheName}}

			err := c.Create(t.Context(), cache)
			if err == nil {
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					if err := c.Delete(ctx, cache); err != nil {
						t.Error(err)
					}
				})
			}

			if tt.wantMessage != "" {
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("create %q: want Invalid containing %q, got %v", tt.cacheName, tt.wantMessage, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("create %q: %v", tt.cacheName, err)
			}

			catalog, err := BuildCatalog([]racerv1.ClusterCache{*cache})
			if err != nil || len(catalog) != 1 {
				t.Fatalf("admitted cache cannot enter wire catalog: %v, %v", catalog, err)
			}
		})
	}
}
