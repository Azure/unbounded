// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/unbounded/internal/racer"
)

func TestNodeMembership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labels   map[string]string
		site     string
		eligible bool
	}{
		{name: "unassigned"},
		{name: "empty labels", labels: map[string]string{}},
		{name: "canonical", labels: map[string]string{racer.SiteLabelKey: "site-a"}, site: "site-a", eligible: true},
		{name: "deprecated", labels: map[string]string{racer.DeprecatedSiteLabelKey: "site-b"}, site: "site-b", eligible: true},
		{name: "conflicting", labels: map[string]string{racer.SiteLabelKey: "site-a", racer.DeprecatedSiteLabelKey: "site-b"}, site: "site-a", eligible: true},
		{name: "canonical empty blocks fallback", labels: map[string]string{racer.SiteLabelKey: "", racer.DeprecatedSiteLabelKey: "site-b"}},
		{name: "deprecated empty", labels: map[string]string{racer.DeprecatedSiteLabelKey: ""}},
		{name: "universe cannot assign Site", labels: map[string]string{racer.UniverseKey: "default"}},
		{name: "excluded canonical", labels: map[string]string{racer.SiteLabelKey: "site-a", racer.ExcludeLabelKey: "true"}, site: "site-a"},
		{name: "excluded deprecated", labels: map[string]string{racer.DeprecatedSiteLabelKey: "site-b", racer.ExcludeLabelKey: "true"}, site: "site-b"},
		{name: "false", labels: map[string]string{racer.SiteLabelKey: "site-a", racer.ExcludeLabelKey: "false"}, site: "site-a", eligible: true},
		{name: "case sensitive", labels: map[string]string{racer.SiteLabelKey: "site-a", racer.ExcludeLabelKey: "True"}, site: "site-a", eligible: true},
		{name: "not boolean parsing", labels: map[string]string{racer.SiteLabelKey: "site-a", racer.ExcludeLabelKey: "1"}, site: "site-a", eligible: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Labels: tc.labels,
				Annotations: map[string]string{
					racer.SiteLabelKey:    "ignored-site",
					racer.UniverseKey:     "ignored-universe",
					racer.ExcludeLabelKey: "true",
				},
			}}
			if got := racer.NodeSite(node); got != tc.site {
				t.Fatalf("Site = %q, want %q", got, tc.site)
			}

			if got := racer.NodeUniverse(node); got != tc.site {
				t.Fatalf("universe = %q, want %q", got, tc.site)
			}

			if got := racer.NodeEligible(node); got != tc.eligible {
				t.Fatalf("eligible = %v, want %v", got, tc.eligible)
			}
		})
	}

	if racer.NodeSite(nil) != "" || racer.NodeUniverse(nil) != "" || racer.NodeEligible(nil) {
		t.Fatal("nil Node must have no Site, universe, or eligibility")
	}
}

func TestUniverseMapping(t *testing.T) {
	const encoded = "site_77qfj7t24dfw3rs4hl43mhksbh2dtbi5wq6qxjmzom356fkgndvq"

	for _, site := range []string{"", "site-a", "site.a", "A_site.1", strings.Repeat("a", 63)} {
		if got := racer.UniverseForSite(site); got != site {
			t.Errorf("legal label %q mapped to %q", site, got)
		}
	}

	if got := racer.UniverseForSite(strings.Repeat("a", 64)); got != encoded {
		t.Fatalf("64-byte name = %q, want stable encoding %q", got, encoded)
	}

	seen := map[string]string{}

	for _, site := range []string{
		strings.Repeat("a", 64), strings.Repeat("a", 63) + "b",
		strings.Repeat("a.", 126) + "a", "-invalid", "invalid-", "a/b", "site name", "東京",
	} {
		got := racer.UniverseForSite(site)
		if got == "" || len(validation.IsValidLabelValue(got)) != 0 {
			t.Fatalf("%q produced invalid universe label %q", site, got)
		}

		if got != racer.UniverseForSite(site) {
			t.Fatalf("%q mapping is not deterministic", site)
		}

		if other, ok := seen[got]; ok {
			t.Fatalf("%q and %q mapped to %q", site, other, got)
		}

		seen[got] = site
		// Hash-backed names cannot alias an unencoded, valid Site resource name.
		if len(validation.IsDNS1123Subdomain(got)) == 0 {
			t.Fatalf("encoded universe %q overlaps the Site resource name namespace", got)
		}

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{racer.SiteLabelKey: site}}}
		if racer.NodeUniverse(node) != got {
			t.Fatalf("Node mapping differs from Site mapping for %q", site)
		}
	}
}

func TestBootstrapIdentity(t *testing.T) {
	for _, tc := range []struct{ site, want string }{
		{"", ""},
		{"site-a", "006b082d36689058fb63d9eaeb8af2aa4dc52210879dd3d2a59b409c90e8675c"},
		{strings.Repeat("a", 64), "01153fb86c130e17c86d292cd7205d455fc4c6dff8084713429b1a074b31c174"},
	} {
		if got := racer.UniverseIDForSite(tc.site); got != tc.want {
			t.Errorf("bootstrap identity for %q = %q, want %q", tc.site, got, tc.want)
		}

		if tc.site != "" && racer.UniverseIDForSite(tc.site) != racer.Identity("universe", racer.UniverseForSite(tc.site)) {
			t.Errorf("bootstrap must hash the Pod/Service universe for %q", tc.site)
		}
	}

	if got := racer.Identity("node", "node-uid"); got != "15be4d45ed47a056073dfd053b9903f3bec1021d8791f13611ce7f424450d6da" {
		t.Fatalf("Node identity changed from source protocol: %q", got)
	}

	if racer.Identity("node", "site-a") == racer.Identity("universe", "site-a") {
		t.Fatal("identity domains must be separated")
	}
}

// Evaluate every OR branch with Kubernetes' label selector implementation, so
// the test checks scheduling semantics rather than just the affinity's shape.
func affinityMatches(t *testing.T, affinity *corev1.NodeAffinity, nodeLabels map[string]string) bool {
	t.Helper()

	if affinity == nil || affinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("required affinity must always be present, even for no Site")
	}

	for _, term := range affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 {
			continue
		}

		selector := &metav1.LabelSelector{}
		for _, expression := range term.MatchExpressions {
			selector.MatchExpressions = append(selector.MatchExpressions, metav1.LabelSelectorRequirement{
				Key: expression.Key, Operator: metav1.LabelSelectorOperator(expression.Operator), Values: expression.Values,
			})
		}

		compiled, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			t.Fatalf("invalid affinity: %v", err)
		}

		if compiled.Matches(labels.Set(nodeLabels)) {
			return true
		}
	}

	return false
}

func TestRequiredNodeAffinity(t *testing.T) {
	// nil is absent; a pointer to empty is present and must block fallback.
	values := []*string{nil, new(""), new("site-a"), new("site-b")}
	exclusions := []*string{nil, new(""), new("true"), new("false"), new("True"), new("1")}

	for _, site := range []string{"site-a", "site-b", "", strings.Repeat("a", 64), "invalid/site"} {
		affinity := racer.RequiredNodeAffinity(site)

		for _, canonical := range values {
			for _, deprecated := range values {
				for _, exclude := range exclusions {
					nodeLabels := map[string]string{}

					for key, value := range map[string]*string{
						racer.SiteLabelKey: canonical, racer.DeprecatedSiteLabelKey: deprecated, racer.ExcludeLabelKey: exclude,
					} {
						if value != nil {
							nodeLabels[key] = *value
						}
					}

					node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: nodeLabels}}

					want := racer.NodeEligible(node) && racer.NodeSite(node) == site
					if got := affinityMatches(t, affinity, nodeLabels); got != want {
						t.Fatalf("Site %q labels %v: affinity matches %v, eligibility requires %v", site, nodeLabels, got, want)
					}
				}
			}
		}
	}
}
