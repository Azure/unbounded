// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterCache names a disposable cache. Its Kubernetes UID is its wire identity.
// Socket paths are derived from metadata.name. Client access is controlled by pod volume mounts.
// The name limit keeps /run/racer/<name>/origin/socket within Linux sockaddr_un.
// Each DNS label is limited to 63 characters to match the wire contract;
// Kubernetes metadata validation enforces DNS subdomain spelling.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rcache
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 82",message="name must fit the canonical Unix socket path"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.split('.').all(label, size(label) <= 63)",message="each name label must be at most 63 characters"
type ClusterCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCache `json:"items"`
}
