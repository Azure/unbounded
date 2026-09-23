// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// NodeSite returns the Node's Site, or empty for nil/unassigned Nodes. Presence
// of the canonical label wins, even when empty; only absence permits fallback.
func NodeSite(node *corev1.Node) string {
	if node == nil {
		return ""
	}

	if site, present := node.Labels[SiteLabelKey]; present {
		return site
	}

	return node.Labels[DeprecatedSiteLabelKey]
}

// UniverseForSite maps one Site name to one stable universe label value. Empty
// means no universe. Legal label values (at most 63 bytes) are preserved;
// otherwise use "site_" plus the lowercase, unpadded base32 SHA-256 of the name.
// The underscore separates encoded names from valid DNS-subdomain Site names.
// This is a label value, not necessarily a valid Kubernetes resource name.
// Pod labels, Service selectors/annotations, and bootstrap must share this map.
func UniverseForSite(site string) string {
	if site == "" || len(validation.IsValidLabelValue(site)) == 0 {
		return site
	}

	checksum := sha256.Sum256([]byte(site))

	return "site_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(checksum[:]))
}

// NodeUniverse returns Site-derived membership, including for excluded Nodes
// whose previous membership may still be needed for cleanup. Use NodeEligible
// to determine whether the Node may actively participate.
func NodeUniverse(node *corev1.Node) string {
	return UniverseForSite(NodeSite(node))
}

// NodeEligible reports Site membership and absence of the exact exclusion value
// "true". Callers additionally enforce availability, OS, and deployment profile.
func NodeEligible(node *corev1.Node) bool {
	return NodeSite(node) != "" && node.Labels[ExcludeLabelKey] != "true"
}

// ValidateBootstrapNode binds a new process to its Pod's explicit mapped
// universe. Existing processes retain their old identity while draining.
func ValidateBootstrapNode(node *corev1.Node, expectedUniverse string) error {
	if node == nil || node.UID == "" {
		return fmt.Errorf("node has no UID")
	}

	if node.DeletionTimestamp != nil || !NodeEligible(node) {
		return fmt.Errorf("node is not eligible for Racer Site membership")
	}

	if expectedUniverse == "" || NodeUniverse(node) != expectedUniverse {
		return fmt.Errorf("node Site universe does not match Pod universe %q", expectedUniverse)
	}

	return nil
}

// Identity returns the hex-encoded, domain-separated SHA-256 identity used by
// Racer's control protocol. For universe identities use UniverseIDForSite, or
// pass an already mapped universe to Identity("universe", universe). Node
// identities use Identity("node", string(node.UID)), never the Node name.
func Identity(domain, value string) string {
	checksum := sha256.Sum256([]byte("racer/" + domain + "/v1\x00" + value))
	return hex.EncodeToString(checksum[:])
}

// UniverseIDForSite returns the bootstrap/control identity of the mapped
// universe, or empty for no Site. It hashes the same value used on Pods/Services.
func UniverseIDForSite(site string) string {
	universe := UniverseForSite(site)
	if universe == "" {
		return ""
	}

	return Identity("universe", universe)
}

// RequiredNodeAffinity selects eligible members of site, with canonical-first
// fallback semantics. Exclusion is ANDed into both OR branches. Empty or invalid
// Site label values match no Nodes. OS/profile selection is left to callers,
// who may AND nodeSelector constraints with this required affinity.
func RequiredNodeAffinity(site string) *corev1.NodeAffinity {
	// Kubernetes requires at least one term. Contradictory requirements in the
	// same AND term are API-valid but match no Node, whether the label is absent,
	// empty, or populated. Never substitute the mapped universe for a Site label.
	selector := &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpExists},
			{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpDoesNotExist},
		}},
	}}
	if site != "" && len(validation.IsValidLabelValue(site)) == 0 {
		selector.NodeSelectorTerms = []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{site}},
				{Key: ExcludeLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
			}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpDoesNotExist},
				{Key: DeprecatedSiteLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{site}},
				{Key: ExcludeLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
			}},
		}
	}

	return &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: selector}
}

// EligibleNodeAffinity selects nonempty canonical Site membership, falling back
// to the deprecated label only when the canonical label is absent.
func EligibleNodeAffinity() *corev1.NodeAffinity {
	return &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpExists},
			{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{""}},
			{Key: ExcludeLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
		}},
		{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: SiteLabelKey, Operator: corev1.NodeSelectorOpDoesNotExist},
			{Key: DeprecatedSiteLabelKey, Operator: corev1.NodeSelectorOpExists},
			{Key: DeprecatedSiteLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{""}},
			{Key: ExcludeLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
		}},
	}}}
}
