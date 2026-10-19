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
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ccache
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 82",message="name must fit the canonical Unix socket path"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[^.]{1,63}([.][^.]{1,63})*$')",message="each name label must be at most 63 characters"
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
	// one origin pod per dataplane node and removes it when origin is unset.
	// +optional
	Origin *ObjectOrigin `json:"origin,omitempty"`
}

// ObjectOrigin configures a managed object storage origin.
// Exactly one provider must be set. Only azure is supported today.
// +kubebuilder:validation:XValidation:rule="has(self.azure)",message="azure is required"
type ObjectOrigin struct {
	// Buckets lists the buckets or containers the origin may read.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Buckets []BucketName `json:"buckets"`

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

	// Azure reads from Azure Blob Storage. Buckets name blob containers.
	// +optional
	Azure *AzureOrigin `json:"azure,omitempty"`
}

// BucketName names an object storage bucket or blob container.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
// +kubebuilder:validation:XValidation:rule="self != '.' && self != '..'",message="bucket must not be . or .."
type BucketName string

// AzureOrigin reads from one Azure Blob Storage account.
// The account URL also forms the cache's upstream identity, so changing it
// starts a new set of cache entries.
// +kubebuilder:validation:XValidation:rule="!has(self.credential) || self.accountURL.startsWith('https://')",message="accountURL must use https when a credential is configured"
type AzureOrigin struct {
	// AccountURL is the blob service endpoint, for example
	// https://account.blob.core.windows.net. Azurite-style URLs may carry one
	// account path segment.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['http', 'https']",message="accountURL must be an http or https URL"
	// +kubebuilder:validation:XValidation:rule="!self.contains('?') && !self.contains('#') && !self.contains('@')",message="accountURL must not contain credentials, a query, or a fragment"
	AccountURL string `json:"accountURL"`

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
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}
