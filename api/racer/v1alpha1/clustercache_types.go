// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterCacheSpec configures a cache's client socket. Origin sockets belong to adapters.
type ClusterCacheSpec struct {
	// SocketMode contains Unix permission bits, expressed as a decimal integer.
	// Paths are derived from metadata.name, never supplied independently.
	// +optional
	// +kubebuilder:default=432
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=511
	SocketMode *int32 `json:"socketMode,omitempty"`
}

// ClusterCache names a disposable cache. Its Kubernetes UID is its wire identity.
// The name limit keeps /run/racer/<name>/origin/socket within Linux sockaddr_un.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rcache
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 82",message="name must fit the canonical Unix socket path"
type ClusterCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterCacheSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCache `json:"items"`
}
