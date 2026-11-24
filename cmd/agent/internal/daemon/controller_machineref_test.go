// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestDaemonCacheOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		selectable bool
	}{
		{name: "selection supported", selectable: true},
		{name: "fallback", selectable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			options := daemonCacheOptions("machine-1", "node-1", tt.selectable)

			var machineOperation *ctrlcache.ByObject

			for obj, byObject := range options.ByObject {
				if _, ok := obj.(*v1alpha3.MachineOperation); ok {
					entry := byObject
					machineOperation = &entry
				}
			}

			if !tt.selectable {
				assert.Nil(t, machineOperation, "the fallback cache must be unfiltered")

				return
			}

			require.NotNil(t, machineOperation)
			require.NotNil(t, machineOperation.Field)
			assert.Equal(t, "spec.machineRef=machine-1", machineOperation.Field.String())
		})
	}
}
