// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Topology-aware HRW candidate selection (§4.3, §8 open question).
//
// HRW core (TopK / Score / RankOf) is topology-agnostic. This file adds
// the candidate-set filter that, depending on configuration, returns
// either the full cluster view or a single-zone slice.

package hrw

import "github.com/Azure/unbounded/internal/gantry/ifaces"

// Scope is the topology mode HRW operates in.
type Scope int

const (
	// ScopeCluster considers every node in the membership view.
	ScopeCluster Scope = 0
	// ScopeZone restricts the candidate set to nodes that share a zone
	// label with the requester. Nodes without a Zone label (Zone == "")
	// are excluded entirely in ScopeZone — they cannot be matched.
	ScopeZone Scope = 1
)

// Candidates filters cluster by scope:
//   - ScopeCluster: returns cluster unchanged (caller may share the
//     backing array — TopK never mutates it).
//   - ScopeZone: returns the subset whose Zone equals requesterZone. If
//     requesterZone == "" the returned slice is empty; the caller is
//     responsible for handling that case (typically by falling back to
//     ScopeCluster behavior at the config layer).
//
// The returned slice is freshly allocated only when filtering is
// necessary.
func Candidates(cluster []ifaces.Node, scope Scope, requesterZone string) []ifaces.Node {
	switch scope {
	case ScopeZone:
		if requesterZone == "" {
			return nil
		}
		out := make([]ifaces.Node, 0, len(cluster))
		for _, n := range cluster {
			if n.Zone == requesterZone {
				out = append(out, n)
			}
		}
		return out
	default:
		return cluster
	}
}

// ParseScope returns the Scope value for a config string. "zone" and
// "cluster" are accepted; everything else is treated as ScopeCluster
// (the safe default per the config layer's Validate step).
func ParseScope(s string) Scope {
	if s == "zone" {
		return ScopeZone
	}
	return ScopeCluster
}
