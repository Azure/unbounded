// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
)

// TestBuildMachineCRCarriesProvisioningFormat covers the declaration a
// controller-driven HostReplace depends on.
//
// The agent is the only party that knows how the host was provisioned: the
// image identifier is opaque, and the running host cannot be probed for it
// because a replacement may change the image. If the Machine does not carry it,
// replacement renders cloud-init for an Ignition host, destroying a working
// node and returning an unprovisioned one.
func TestBuildMachineCRCarriesProvisioningFormat(t *testing.T) {
	t.Parallel()

	cfg := &provision.AgentConfig{
		MachineName:        "node-1",
		ProvisioningFormat: config.ProvisioningFormatIgnition,
		Kubelet: provision.AgentKubeletConfig{
			Auth: provision.KubeletAuthInfo{BootstrapToken: "abc123.secret456"},
		},
	}

	machine := buildMachineCR(cfg)

	require.NotNil(t, machine.Spec.Host)
	assert.Equal(t, v1alpha3.ProvisioningFormatIgnition, machine.Spec.Host.ProvisioningFormat)
}

// TestBuildMachineCRLeavesCloudInitUnset keeps hosts that predate the field
// working: unset already means cloud-init, and writing it out would suggest a
// declaration the host never made.
func TestBuildMachineCRLeavesCloudInitUnset(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"", config.ProvisioningFormatCloudInit} {
		cfg := &provision.AgentConfig{
			MachineName:        "node-1",
			ProvisioningFormat: format,
			Kubelet: provision.AgentKubeletConfig{
				Auth: provision.KubeletAuthInfo{BootstrapToken: "abc123.secret456"},
			},
		}

		machine := buildMachineCR(cfg)

		assert.Nil(t, machine.Spec.Host,
			"an undeclared or cloud-init host must not gain a host spec")
	}
}
