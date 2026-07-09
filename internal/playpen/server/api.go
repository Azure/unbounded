// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import "time"

const (
	GroupName    = "playpen.unbounded-cloud.io"
	Version      = "v1alpha1"
	GroupVersion = GroupName + "/" + Version

	AllocatePath   = "/apis/" + GroupVersion + "/vms/allocate"
	DeallocatePath = "/apis/" + GroupVersion + "/vms/deallocate"
)

type AllocateRequest struct {
	NamePrefix               string `json:"namePrefix,omitempty"`
	Site                     string `json:"site,omitempty"`
	GatewayPool              string `json:"gatewayPool,omitempty"`
	ClientWireGuardPublicKey string `json:"clientWireGuardPublicKey,omitempty"`
	ClientInternalIP         string `json:"clientInternalIP,omitempty"`
	PodCIDR                  string `json:"podCIDR,omitempty"`
	VMImage                  string `json:"vmImage,omitempty"`
	NetworkAttachmentName    string `json:"networkAttachmentName,omitempty"`
	SSHAuthorizedKey         string `json:"sshAuthorizedKey,omitempty"`
	TTLSeconds               int64  `json:"ttlSeconds,omitempty"`
}

type AllocateResponse struct {
	AllocationID     string        `json:"allocationID"`
	Namespace        string        `json:"namespace"`
	VMName           string        `json:"vmName"`
	NodeName         string        `json:"nodeName"`
	Site             string        `json:"site"`
	PodCIDR          string        `json:"podCIDR"`
	ExpiresAt        time.Time     `json:"expiresAt"`
	MACAddress       string        `json:"macAddress"`
	Lease            DHCPLease     `json:"lease"`
	Redfish          RedfishAccess `json:"redfish"`
	Tunnel           TunnelInfo    `json:"tunnel"`
	L2Tunnel         L2TunnelInfo  `json:"l2Tunnel,omitempty"`
	RequiresEndpoint bool          `json:"requiresEndpoint"`
	GatewayPeers     []GatewayPeer `json:"gatewayPeers,omitempty"`
}

type DeallocateRequest struct {
	AllocationID string `json:"allocationID"`
}

type DeallocateResponse struct {
	AllocationID string `json:"allocationID"`
	Deleted      bool   `json:"deleted"`
}

type DHCPLease struct {
	IP     string `json:"ip"`
	Subnet string `json:"subnet"`
	Router string `json:"router,omitempty"`
	DNS    string `json:"dns,omitempty"`
}

type RedfishAccess struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	DeviceID string `json:"deviceID"`
}

type TunnelInfo struct {
	Mode                string `json:"mode"`
	WireGuardAddress    string `json:"wireGuardAddress,omitempty"`
	WireGuardPrivateKey string `json:"wireGuardPrivateKey,omitempty"`
	WireGuardPublicKey  string `json:"wireGuardPublicKey,omitempty"`
	WireGuardListenPort int    `json:"wireGuardListenPort,omitempty"`
	VXLANVNI            int    `json:"vxlanVNI"`
	VXLANPort           int    `json:"vxlanPort"`
	NetworkNamespace    string `json:"networkNamespace,omitempty"`
	EndpointRequired    bool   `json:"endpointRequired"`
}

type L2TunnelInfo struct {
	Enabled               bool   `json:"enabled"`
	Mode                  string `json:"mode,omitempty"`
	NetworkAttachmentName string `json:"networkAttachmentName,omitempty"`
	EndpointNamespace     string `json:"endpointNamespace,omitempty"`
	EndpointPodName       string `json:"endpointPodName,omitempty"`
	EndpointUnderlayIP    string `json:"endpointUnderlayIP,omitempty"`
	ClientUnderlayIP      string `json:"clientUnderlayIP,omitempty"`
	VXLANVNI              int    `json:"vxlanVNI,omitempty"`
	VXLANPort             int    `json:"vxlanPort,omitempty"`
	BridgeInterface       string `json:"bridgeInterface,omitempty"`
	AttachInterface       string `json:"attachInterface,omitempty"`
	VXLANInterface        string `json:"vxlanInterface,omitempty"`
}

type GatewayPeer struct {
	Name               string   `json:"name"`
	Site               string   `json:"site,omitempty"`
	WireGuardPublicKey string   `json:"wireGuardPublicKey"`
	InternalIPs        []string `json:"internalIPs,omitempty"`
	Endpoints          []string `json:"endpoints,omitempty"`
	PodCIDRs           []string `json:"podCIDRs,omitempty"`
	RoutedCIDRs        []string `json:"routedCIDRs,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
