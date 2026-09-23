// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer defines metadata and Site identity shared by Racer controllers,
// bootstrap, and operator resources.
package racer

// MetadataPrefix is the namespace for Racer-owned labels and annotations.
const MetadataPrefix = "racer.unbounded-cloud.io/"

// Node membership is read from Site labels, never from a Racer universe label
// or annotation. UniverseKey is the mapped universe on managed Pods.
const (
	SiteLabelKey              = "unbounded-cloud.io/site"
	DeprecatedSiteLabelKey    = "net.unbounded-cloud.io/site"
	DataplaneLabelKey         = MetadataPrefix + "dataplane"
	UniverseKey               = MetadataPrefix + "universe"
	ExcludeLabelKey           = MetadataPrefix + "exclude"
	DeploymentProfileLabelKey = MetadataPrefix + "deployment-profile"
	StateLabelKey             = MetadataPrefix + "state"
	StateOwnerLabelKey        = MetadataPrefix + "state-owner"
)

// Node fabric, storage configuration, and controller-owned storage status.
const (
	FabricAnnotationKey      = MetadataPrefix + "fabric"
	CacheSizeAnnotationKey   = MetadataPrefix + "cache-size"
	CacheStatusAnnotationKey = MetadataPrefix + "cache-status"
)
