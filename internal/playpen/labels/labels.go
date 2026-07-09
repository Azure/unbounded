// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package labels

const (
	AppName = "playpen"

	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ComponentLabel    = "app.kubernetes.io/component"
	AllocationIDLabel = "playpen.unbounded-cloud.io/allocation-id"
	OwnedLabel        = "playpen.unbounded-cloud.io/owned"

	ExpiresAtAnnotation       = "playpen.unbounded-cloud.io/expires-at"
	MACAddressAnnotation      = "playpen.unbounded-cloud.io/mac-address"
	RedfishUsernameAnnotation = "playpen.unbounded-cloud.io/redfish-username"
	BootTargetAnnotation      = "playpen.unbounded-cloud.io/boot-target"
	BootEnabledAnnotation     = "playpen.unbounded-cloud.io/boot-enabled"
	BootModeAnnotation        = "playpen.unbounded-cloud.io/boot-mode"
	HTTPBootURIAnnotation     = "playpen.unbounded-cloud.io/http-boot-uri"
	SiteAnnotation            = "playpen.unbounded-cloud.io/site"
	PodCIDRAnnotation         = "playpen.unbounded-cloud.io/pod-cidr"
	L2TunnelAnnotation        = "playpen.unbounded-cloud.io/l2-tunnel"

	NetSiteLabel              = "net.unbounded-cloud.io/site"
	WireGuardPubKeyAnnotation = "net.unbounded-cloud.io/wg-pubkey"
)
