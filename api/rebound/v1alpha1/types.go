// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeGrant reserves a set of nodes for the namespace it is created in.
// A grant is created atomically: either every listed node is granted or
// the request fails and nothing changes. Grants are immutable and expire
// at status.expiresAt.
//
// +kubebuilder:object:root=true
type NodeGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeGrantSpec   `json:"spec"`
	Status NodeGrantStatus `json:"status,omitempty"`
}

// NodeGrantSpec is the requested grant.
type NodeGrantSpec struct {
	// Nodes is the set of node names to grant. All nodes must exist and be
	// free, or the grant is rejected.
	Nodes []string `json:"nodes"`

	// TTL is how long the grant lasts. It defaults to, and may not exceed,
	// the server's maximum TTL.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// NodeGrantStatus is set by the server.
type NodeGrantStatus struct {
	// Holder is the user that created the grant.
	Holder string `json:"holder,omitempty"`

	// ExpiresAt is when the grant ends.
	ExpiresAt metav1.Time `json:"expiresAt,omitempty"`
}

// NodeGrantList is a list of NodeGrant.
//
// +kubebuilder:object:root=true
type NodeGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeGrant `json:"items"`
}
