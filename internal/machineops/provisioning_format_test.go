// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestReplacementProvisioningFormat(t *testing.T) {
	t.Parallel()

	machine := &api.Machine{}
	format, err := replacementProvisioningFormat(machine, "")
	require.NoError(t, err)
	require.Equal(t, api.ProvisioningFormatCloudInit, format)

	machine.Status.ObservedProvisioningFormat = api.ProvisioningFormatIgnition
	format, err = replacementProvisioningFormat(machine, "")
	require.NoError(t, err)
	require.Equal(t, api.ProvisioningFormatIgnition, format)

	_, err = replacementProvisioningFormat(machine, "/opaque/new-image")
	require.ErrorContains(t, err, "explicit target")

	machine.Spec.Host = &api.HostSpec{ProvisioningFormat: api.ProvisioningFormatCloudInit}
	format, err = replacementProvisioningFormat(machine, "/opaque/new-image")
	require.NoError(t, err)
	require.Equal(t, api.ProvisioningFormatCloudInit, format)
}
