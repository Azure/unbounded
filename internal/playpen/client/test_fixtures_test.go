// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"strings"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

type fakeCommander struct {
	commands []string
}

func (f *fakeCommander) Run(_ context.Context, name string, args ...string) error {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))

	return nil
}

func testAllocResponse() operator.AllocResponse {
	return operator.AllocResponse{
		ID:           "alloc1",
		Architecture: operator.ArchitectureAMD64,
		Endpoint:     operator.EndpointResponse{Host: "20.30.40.50", WireGuardUDPPort: 32000},
		WireGuard: operator.WireGuardResponse{
			Interface:       "wg0",
			ServerPublicKey: "kX4Z6LwejXzAl2m4nA1rY3EWB3yJe2rZXYc2umY7jU0=",
			ServerAddress:   "10.88.0.1/24",
			ClientAddress:   "10.88.0.2/32",
		},
		VXLAN: operator.VXLANResponse{Interface: "vxlan0", VNI: 12001, UDPPort: 4789, ServerAddress: "10.88.0.1", ClientAddress: "10.88.0.2"},
		Network: operator.NetworkResponse{
			GuestMAC:    "02:00:00:00:00:01",
			GuestIPv4:   "192.168.200.10",
			SubnetMask:  "255.255.255.0",
			GatewayIPv4: "192.168.200.1",
		},
		Redfish: map[string]string{
			"url":                    "https://10.88.0.1:8443",
			"username":               "admin",
			"password":               "secret",
			"serialConsoleStreamURI": "/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream",
		},
	}
}
