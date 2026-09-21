// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer defines metadata and Site identity shared by Racer controllers,
// bootstrap, and operator resources.
package racer

// MetadataPrefix is the namespace for Racer-owned labels and annotations.
const MetadataPrefix = "racer.unbounded-cloud.io/"

// Node membership is read from Site labels, never from a Racer universe label
// or annotation. UniverseKey is the mapped universe on Pods and Services.
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

// Service configuration, Node fabric, and controller-owned output annotations.
const (
	OriginServiceAnnotationKey        = MetadataPrefix + "origin-service"
	OriginNamespaceAnnotationKey      = MetadataPrefix + "origin-namespace"
	OriginPortAnnotationKey           = MetadataPrefix + "origin-port"
	FabricAnnotationKey               = MetadataPrefix + "fabric"
	SlotCountAnnotationKey            = MetadataPrefix + "slot-count"
	ListenerPortAnnotationKey         = MetadataPrefix + "listener-port"
	CacheGenerationAnnotationKey      = MetadataPrefix + "cache-generation"
	RoutingAlgorithmAnnotationKey     = MetadataPrefix + "routing-algorithm"
	MaxCandidateAttemptsAnnotationKey = MetadataPrefix + "max-candidate-attempts"
	LegacyPeerWireAnnotationKey       = MetadataPrefix + "legacy-peer-wire"
	AllocatedPortAnnotationKey        = MetadataPrefix + "allocated-port"
	UniverseIDAnnotationKey           = MetadataPrefix + "universe-id"
	StatusAnnotationKey               = MetadataPrefix + "status"
)
