// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package indexing

import (
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// IndexNodeByMAC is the field index key used to look up Machines by MAC address.
const IndexNodeByMAC = ".spec.pxe.dhcpLeases.mac"

// IndexNodeByMACFunc is the field indexer function for IndexNodeByMAC.
func IndexNodeByMACFunc(obj client.Object) []string {
	node := obj.(*v1alpha3.Machine) //nolint:errcheck // Indexer is registered for Machine type only.
	if node.Spec.PXE == nil {
		return nil
	}

	macs := make([]string, 0, len(node.Spec.PXE.DHCPLeases))
	for _, lease := range node.Spec.PXE.DHCPLeases {
		macs = append(macs, strings.ToLower(lease.MAC))
	}

	return macs
}

// IndexNodeByIP is the field index key used to look up Machines by IPv4 address.
const IndexNodeByIP = ".spec.pxe.dhcpLeases.ipv4"

// IndexNodeByIPFunc is the field indexer function for IndexNodeByIP.
func IndexNodeByIPFunc(obj client.Object) []string {
	node := obj.(*v1alpha3.Machine) //nolint:errcheck // Indexer is registered for Machine type only.
	if node.Spec.PXE == nil {
		return nil
	}

	ips := make([]string, 0, len(node.Spec.PXE.DHCPLeases))
	for _, lease := range node.Spec.PXE.DHCPLeases {
		if lease.IPv4 != "" {
			ips = append(ips, lease.IPv4)
		}
	}

	return ips
}
