// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateProvisioningFormat(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"", "cloud-init", "ignition", " ignition "} {
		require.NoError(t, ValidateProvisioningFormat(format))
	}

	for _, format := range []string{"CloudInit", "unknown", "cloudinit"} {
		require.Error(t, ValidateProvisioningFormat(format))
	}
}
