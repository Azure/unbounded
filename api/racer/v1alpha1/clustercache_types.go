// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterCache names a cache. Its Kubernetes UID is its wire identity.
// Socket paths are derived from metadata.name. Client access is controlled by pod volume mounts.
// The name limit keeps /run/racer/<name>/origin/socket within Linux sockaddr_un.
// Each DNS label is limited to 63 characters to match the wire contract;
// Kubernetes metadata validation enforces DNS subdomain spelling.
// The bounded regex checks label lengths without a costly CEL split/all loop.
// Whether spec.origin is set is fixed at creation. Cache entries are keyed by
// the ClusterCache UID, not by their source, so removing and re-adding origin,
// or switching between a managed origin and a self-deployed one, would serve
// entries fetched from a different source.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ccache
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 82",message="name must fit the canonical Unix socket path"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[^.]{1,63}([.][^.]{1,63})*$')",message="each name label must be at most 63 characters"
// +kubebuilder:validation:XValidation:rule="(has(self.spec) && has(self.spec.origin)) == (has(oldSelf.spec) && has(oldSelf.spec.origin))",message="spec.origin cannot be added or removed; create a new ClusterCache to change the origin"
type ClusterCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec configures optional cache features.
	// +optional
	Spec ClusterCacheSpec `json:"spec,omitempty,omitzero"`
}

// ClusterCacheSpec configures optional cache features.
type ClusterCacheSpec struct {
	// Origin deploys a managed racer-object origin for this cache. The operator runs
	// one origin pod per dataplane node. Origin can be set only at creation and
	// cannot be removed later; leave it unset to deploy your own origin.
	// +optional
	Origin *ObjectOrigin `json:"origin,omitempty"`

	// Priority is the relative weight used to share node cache capacity among
	// ClusterCaches. Disk shares are weighted across published caches, plus any
	// unpublished caches that still hold data on the node, which count at the
	// default weight. Memory shares are weighted across caches that are
	// actively using memory, and each cache also gets a bounded working
	// allowance from shared memory. Eviction prefers data from caches that use
	// more than their share, within bounded scans. A priority of 0 gets no
	// weighted share while caches with positive priority compete, but it keeps
	// its working allowance. If every counted cache has priority 0, they share
	// equally. When unset, the priority is 500. Priority may be changed at any
	// time.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// Passthrough, when set, turns off caching for this cache: Racer proxies
	// reads to the origin and keeps no data. Pages are not written to disk and
	// are held in memory only while concurrent readers share an in-flight
	// origin read. Object metadata (ETag, size, expiry) is not reused across
	// reads, so each read revalidates with the origin; one read still pins one
	// object version for all of its pages. Warm and hold (pinning) requests are
	// rejected. Priority still weights in-flight memory use. Passthrough may be
	// added, changed, or removed at any time; when a cache switches to
	// passthrough, data previously cached for it is dropped.
	// +optional
	Passthrough *Passthrough `json:"passthrough,omitempty"`
}

// PassthroughRoute selects which node's origin serves a passthrough read.
// +kubebuilder:validation:Enum=Peers;Local
type PassthroughRoute string

const (
	// PassthroughRoutePeers routes each page to its owner nodes through the
	// mesh, as for a cached cache. The owner calls its local origin, and
	// concurrent reads of the same page anywhere in the cluster share one
	// origin read (singleflight), with many pages in flight at once
	// (pipelining). Nothing is retained after in-flight readers finish.
	PassthroughRoutePeers PassthroughRoute = "Peers"
	// PassthroughRouteLocal makes the reading node call its own local origin
	// directly; it never asks peers. Concurrent reads of the same page on one
	// node still share one origin read.
	PassthroughRouteLocal PassthroughRoute = "Local"
)

// Passthrough configures a cache that proxies reads without caching.
type Passthrough struct {
	// Route selects which node's origin serves a read. Peers routes pages to
	// their owner nodes and shares origin reads cluster-wide. Local reads from
	// the reading node's own origin, so every reading node needs an origin
	// (a managed spec.origin runs one per dataplane node). When unset, Peers.
	// +kubebuilder:default=Peers
	// +optional
	Route PassthroughRoute `json:"route,omitempty"`
}

// EffectivePassthroughRoute reports whether passthrough is enabled and its
// route, defaulting an empty route to PassthroughRoutePeers.
func (s ClusterCacheSpec) EffectivePassthroughRoute() (PassthroughRoute, bool) {
	if s.Passthrough == nil {
		return "", false
	}

	if s.Passthrough.Route == "" {
		return PassthroughRoutePeers, true
	}

	return s.Passthrough.Route, true
}

const (
	// MinPriority is the lowest allowed ClusterCache priority.
	MinPriority int32 = 0
	// MaxPriority is the highest allowed ClusterCache priority.
	MaxPriority int32 = 1000
	// DefaultPriority applies when spec.priority is unset.
	DefaultPriority int32 = 500
)

// EffectivePriority returns spec.priority, or DefaultPriority when unset.
func (s ClusterCacheSpec) EffectivePriority() int32 {
	if s.Priority == nil {
		return DefaultPriority
	}

	return *s.Priority
}

// ObjectOrigin configures a managed object storage origin.
// Exactly one provider must be set. Only azure is supported today.
// +kubebuilder:validation:XValidation:rule="has(self.azure)",message="azure is required"
type ObjectOrigin struct {
	// MetadataTTL bounds how long object metadata is reused before it is revalidated.
	// It must not be negative. When unset, the racer-object default applies.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="metadataTTL must not be negative"
	// +optional
	MetadataTTL *metav1.Duration `json:"metadataTTL,omitempty"`

	// RequestTimeout bounds each upstream request. It must be positive.
	// When unset, the racer-object default applies.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="requestTimeout must be positive"
	// +optional
	RequestTimeout *metav1.Duration `json:"requestTimeout,omitempty"`

	// MaxConcurrentRequests bounds concurrent upstream requests per origin pod.
	// When unset, the racer-object default applies.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65536
	// +optional
	MaxConcurrentRequests *int32 `json:"maxConcurrentRequests,omitempty"`

	// Azure reads one Azure Blob Storage container.
	// +optional
	Azure *AzureOrigin `json:"azure,omitempty"`
}

// AzureOrigin reads one Azure Blob Storage container. Every pod attached to
// the cache sees the container's whole keyspace. Cache entries do not record
// the account or container, so the container URL is immutable: a different
// source needs a new ClusterCache, which has a new UID and therefore an empty
// cache.
// +kubebuilder:validation:XValidation:rule="!has(self.credential) || self.containerURL.startsWith('https://')",message="containerURL must use https when a credential is configured"
type AzureOrigin struct {
	// ContainerURL is the blob container URL, for example
	// https://account.blob.core.windows.net/models. The last path segment is
	// the container name. Azurite-style URLs may carry one account path
	// segment before it, for example http://127.0.0.1:10000/account/models.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=320
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['http', 'https']",message="containerURL must be an http or https URL"
	// +kubebuilder:validation:XValidation:rule="!self.contains('?') && !self.contains('#') && !self.contains('@')",message="containerURL must not contain credentials, a query, or a fragment"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https?://[]A-Za-z0-9.:[-]+(/[A-Za-z0-9_-]+)?/[a-z0-9]+(-[a-z0-9]+)*$') && self.substring(self.lastIndexOf('/') + 1).size() >= 3 && self.substring(self.lastIndexOf('/') + 1).size() <= 63",message="containerURL must be an account URL followed by one valid container name, such as https://account.blob.core.windows.net/models"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="containerURL is immutable; create a new ClusterCache for a different container"
	ContainerURL string `json:"containerURL"`

	// Credential selects how the origin authenticates. When omitted, requests are anonymous.
	// +optional
	Credential *AzureCredential `json:"credential,omitempty"`
}

// AzureCredential selects one Azure authentication method.
// +kubebuilder:validation:XValidation:rule="[has(self.default), has(self.workloadIdentity)].filter(x, x).size() == 1",message="exactly one of default or workloadIdentity is required"
type AzureCredential struct {
	// Default uses DefaultAzureCredential, such as a managed identity or
	// service principal environment variables from a Secret.
	// +optional
	Default *AzureDefaultCredential `json:"default,omitempty"`

	// WorkloadIdentity uses Microsoft Entra Workload ID. The cluster must run
	// the Azure Workload Identity webhook, and the identity must trust the
	// origin's ServiceAccount.
	// +optional
	WorkloadIdentity *AzureWorkloadIdentityCredential `json:"workloadIdentity,omitempty"`
}

// AzureDefaultCredential configures DefaultAzureCredential.
// +kubebuilder:validation:XValidation:rule="!(has(self.clientID) && has(self.secretRef))",message="clientID and secretRef are mutually exclusive"
type AzureDefaultCredential struct {
	// ClientID selects a user-assigned managed identity.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	// +optional
	ClientID string `json:"clientID,omitempty"`

	// SecretRef names a Secret in the operator namespace whose keys are
	// injected as environment variables, for example AZURE_CLIENT_ID,
	// AZURE_TENANT_ID, and AZURE_CLIENT_SECRET.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

// AzureWorkloadIdentityCredential configures Microsoft Entra Workload ID.
type AzureWorkloadIdentityCredential struct {
	// ClientID is the client ID of the federated identity.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	ClientID string `json:"clientID"`

	// TenantID overrides the tenant configured by the webhook.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	// +optional
	TenantID string `json:"tenantID,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCache `json:"items"`
}

// SecretReference names a Secret in the operator namespace.
type SecretReference struct {
	// Name of the Secret. It must be a DNS-1123 subdomain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}
