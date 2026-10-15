// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ociinstance

import (
	"testing"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/machineops"
)

func TestResolveImageID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		instance core.Instance
		image    string
		want     string
	}{
		{name: "explicit image", image: " new-image ", want: "new-image"},
		{name: "current source value", instance: core.Instance{SourceDetails: core.InstanceSourceViaImageDetails{ImageId: ptrTo("source-image")}, ImageId: ptrTo("old-image")}, want: "source-image"},
		{name: "current source pointer", instance: core.Instance{SourceDetails: &core.InstanceSourceViaImageDetails{ImageId: ptrTo("source-image")}}, want: "source-image"},
		{name: "current image", instance: core.Instance{ImageId: ptrTo("old-image")}, want: "old-image"},
		{name: "whitespace preserves current image", image: " \t", instance: core.Instance{ImageId: ptrTo("old-image")}, want: "old-image"},
		{name: "missing image"},
		{name: "empty source", instance: core.Instance{SourceDetails: core.InstanceSourceViaImageDetails{ImageId: ptrTo(" ")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveImageID(tc.instance, machineops.OperationRequest{HostImage: tc.image, Parameters: map[string]string{"imageID": "removed-override"}})
			if tc.want == "" {
				require.ErrorContains(t, err, "set Machine spec.host.image")
				require.NotContains(t, err.Error(), "parameters")

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
