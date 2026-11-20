// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterCacheFill pre-fetches a set of objects into a ClusterCache and keeps
// them resident on disk. Admission checks the planned footprint against the
// fill's own limit, not available cluster capacity. Nodes pin pages on a best
// effort basis within their local budgets; pinned pages are not evicted to
// make room for other data. Fills cover caches that
// read directly from object storage (spec.origin); caches served by a
// self-deployed origin, and passthrough caches, are rejected.
//
// A fill names blob-name prefixes, not Racer keys. Racer keys are hashes and
// have no meaningful prefix, so the control plane lists the cache's origin to
// expand the prefixes into object names before it pins anything.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ccfill
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cache",type=string,JSONPath=`.spec.cacheName`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Footprint",type=integer,JSONPath=`.status.footprintBytes`
// +kubebuilder:printcolumn:name="Resident",type=integer,JSONPath=`.status.residentBytes`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Degraded",type=string,JSONPath=`.status.conditions[?(@.type=="Degraded")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterCacheFill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec selects the cache, the origin objects to pin, and the reservation.
	// +required
	Spec ClusterCacheFillSpec `json:"spec"`

	// Status reports the reservation and its progress. It is written by the
	// Racer control plane.
	// +optional
	Status ClusterCacheFillStatus `json:"status,omitempty,omitzero"`
}

// ClusterCacheFillSpec selects the cache, the origin objects to pin, and the
// reservation.
type ClusterCacheFillSpec struct {
	// CacheName names the ClusterCache to fill. The cache must set
	// spec.origin and must not set spec.passthrough. Changing the cache
	// requires a new ClusterCacheFill.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=82
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cacheName is immutable; create a new ClusterCacheFill for a different cache"
	CacheName string `json:"cacheName"`

	// Prefixes are blob-name prefixes in the cache's container. Every object
	// whose name starts with one of them is pinned, including objects added
	// later on the next resync. An empty string selects the whole container.
	// Prefixes are blob names, not Racer keys. At most 16 prefixes, and a
	// single fill lists at most 8192 objects; a larger selection is rejected
	// rather than silently truncated.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=1024
	// +listType=set
	Prefixes []string `json:"prefixes"`

	// Replicas is the number of pinned copies of each page, across distinct
	// Racer nodes. A fill with more replicas survives the loss of up to
	// replicas-1 nodes without going to the origin. Only values from 1 to 3
	// are accepted because Racer keeps at most three owners per page. When
	// unset, 2.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	// +kubebuilder:default=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// MaxFootprint is the largest reserved pinned payload, in bytes, that the
	// fill may plan cluster-wide. It counts every page of every replica, with
	// each object rounded up to a whole page, so it bounds the planned
	// reservation rather than the object size. Actual disk use can be higher
	// because a pinned segment keeps its unused tail; the per-worker pin
	// budget bounds that overhead. A larger selection fails admission with
	// ExceedsMaxFootprint; it is never truncated. This limit does not reserve
	// cluster capacity.
	// +kubebuilder:validation:Minimum=1
	MaxFootprint int64 `json:"maxFootprint"`

	// ResyncInterval bounds how long the listed object set may go stale. It
	// must be between 1m and 24h. Missing pages are retried on a short
	// backoff independently. When unset, 1h.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('60s') && duration(self) <= duration('86400s')",message="resyncInterval must be between 1m and 24h"
	// +kubebuilder:default="1h"
	// +optional
	ResyncInterval *metav1.Duration `json:"resyncInterval,omitempty"`
}

const (
	// DefaultFillReplicas applies when spec.replicas is unset.
	DefaultFillReplicas int32 = 2
	// MaxFillReplicas is the highest useful replica count. Racer keeps at most
	// this many owners per page.
	MaxFillReplicas int32 = 3
	// DefaultFillResyncInterval applies when spec.resyncInterval is unset.
	DefaultFillResyncInterval int64 = 1 * 60 * 60
	// MinFillResyncInterval is the shortest allowed resync interval, in seconds.
	MinFillResyncInterval int64 = 60
	// MaxFillResyncInterval is the longest allowed resync interval, in seconds.
	MaxFillResyncInterval int64 = 24 * 60 * 60
	// MaxFillObjects bounds one fill's listed object set. Every Racer process
	// lists the selection independently and holds it in memory, so a larger
	// selection is rejected with TooManyObjects instead of being truncated.
	MaxFillObjects int32 = 8192
	// MaxFillPrefixes bounds spec.prefixes.
	MaxFillPrefixes = 16
)

// EffectiveReplicas returns spec.replicas, or DefaultFillReplicas when unset
// or outside the supported range.
func (s ClusterCacheFillSpec) EffectiveReplicas() int32 {
	if s.Replicas < 1 || s.Replicas > MaxFillReplicas {
		return DefaultFillReplicas
	}

	return s.Replicas
}

// EffectiveResyncInterval returns spec.resyncInterval, or
// DefaultFillResyncInterval when unset or outside the supported range.
func (s ClusterCacheFillSpec) EffectiveResyncInterval() int64 {
	if s.ResyncInterval == nil {
		return DefaultFillResyncInterval
	}

	seconds := int64(s.ResyncInterval.Duration / 1e9)
	if seconds < MinFillResyncInterval || seconds > MaxFillResyncInterval {
		return DefaultFillResyncInterval
	}

	return seconds
}

// ClusterCacheFillStatus reports the reservation and its progress.
type ClusterCacheFillStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CacheUID records the ClusterCache UID the manifest was listed against.
	// A different UID means the fill must list and pin from scratch.
	// +optional
	CacheUID string `json:"cacheUID,omitempty"`

	// Manifest describes the listed object set.
	// +optional
	Manifest *FillManifestStatus `json:"manifest,omitempty"`

	// FootprintBytes is the page-rounded pinned payload the fill reserves
	// cluster-wide, in bytes, including every replica. While Accepted is true
	// it is at most spec.maxFootprint.
	// +optional
	FootprintBytes int64 `json:"footprintBytes,omitempty"`

	// ResidentBytes is the page-rounded pinned payload size, in bytes, summed
	// over every confirmed copy. It reaches FootprintBytes when the fill is
	// Ready.
	// +optional
	ResidentBytes int64 `json:"residentBytes,omitempty"`

	// UnderReplicatedBytes is max(0, FootprintBytes - ResidentBytes), including
	// copies missing because their owners no longer report progress.
	// +optional
	UnderReplicatedBytes int64 `json:"underReplicatedBytes,omitempty"`

	// Nodes is the number of member nodes that reported progress for the
	// current manifest and membership.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// Conditions describe admission, listing, and residency. Active condition
	// types are Accepted, Listed, Progressing, Ready, and Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FillManifestStatus describes the listed object set.
type FillManifestStatus struct {
	// Objects is the number of listed objects.
	// +optional
	Objects int32 `json:"objects,omitempty"`

	// LogicalBytes is the summed size of the listed objects, before padding,
	// encryption, or replication.
	// +optional
	LogicalBytes int64 `json:"logicalBytes,omitempty"`

	// Digest identifies the listed object set. Pages are pinned against a
	// digest, so a resync that changes objects rolls to a new manifest.
	// +optional
	Digest string `json:"digest,omitempty"`

	// ListedAt is when the object set was last listed.
	// +optional
	ListedAt *metav1.Time `json:"listedAt,omitempty"`
}

// Condition types written to ClusterCacheFill status.
const (
	// FillConditionAccepted is True when the fill is a supported cache and its
	// planned footprint fits spec.maxFootprint. It does not promise that nodes
	// have enough capacity to pin the plan.
	FillConditionAccepted = "Accepted"
	// FillConditionListed is True when the current spec generation has a
	// complete manifest.
	FillConditionListed = "Listed"
	// FillConditionProgressing is True while the first fill has not completed.
	FillConditionProgressing = "Progressing"
	// FillConditionReady is True when every page has EffectiveReplicas
	// confirmed copies on its current owners.
	FillConditionReady = "Ready"
	// FillConditionDegraded is True after a fill was Ready and is now short of
	// copies, for example after a node was replaced.
	FillConditionDegraded = "Degraded"
)

// Reasons for ClusterCacheFill conditions.
const (
	FillReasonCacheNotFound        = "CacheNotFound"
	FillReasonUnsupportedCache     = "UnsupportedCache"
	FillReasonUnsupportedOrigin    = "UnsupportedOrigin"
	FillReasonExceedsMaxFootprint  = "ExceedsMaxFootprint"
	FillReasonTooManyObjects       = "TooManyObjects"
	FillReasonInsufficientCapacity = "InsufficientCapacity"
	FillReasonListFailed           = "ListFailed"
	FillReasonForbidden            = "Forbidden"
	FillReasonFilling              = "Filling"
	FillReasonComplete             = "Complete"
	FillReasonMemberLost           = "MemberLost"
	FillReasonRefilling            = "Refilling"
	FillReasonCapacityLost         = "CapacityLost"
	FillReasonOriginUnavailable    = "OriginUnavailable"
)

// +kubebuilder:object:root=true
type ClusterCacheFillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCacheFill `json:"items"`
}
