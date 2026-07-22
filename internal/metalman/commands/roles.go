// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

type metalmanRole string

const (
	metalmanRoleController metalmanRole = "controller"
	metalmanRoleServer     metalmanRole = "server"
	metalmanRoleEdge       metalmanRole = "edge"
	metalmanRoleLegacy     metalmanRole = "serve-pxe"
)

type roleComponents struct {
	leaderElection bool
	ociReconciler  bool
	redfish        bool
	machineOps     bool
	dhcp           bool
	tftp           bool
	http           bool
	attestation    bool
	statusUpdates  bool
}

func componentsForRole(role metalmanRole) roleComponents {
	switch role {
	case metalmanRoleController:
		return roleComponents{
			leaderElection: true,
			ociReconciler:  true,
			redfish:        true,
			machineOps:     true,
		}
	case metalmanRoleServer:
		return roleComponents{
			ociReconciler: true,
			http:          true,
			attestation:   true,
			statusUpdates: true,
		}
	case metalmanRoleEdge:
		return roleComponents{
			ociReconciler: true,
			dhcp:          true,
			tftp:          true,
			http:          true,
			attestation:   true,
			statusUpdates: true,
		}
	case metalmanRoleLegacy:
		return roleComponents{
			leaderElection: true,
			ociReconciler:  true,
			redfish:        true,
			machineOps:     true,
			dhcp:           true,
			tftp:           true,
			http:           true,
			attestation:    true,
			statusUpdates:  true,
		}
	default:
		return roleComponents{}
	}
}
