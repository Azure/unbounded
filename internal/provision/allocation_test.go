// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestAllocationUID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		configuration map[string]any
		wantUID       string
		wantBacked    bool
	}{
		{name: "prefix with uid", configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + "abc"}, wantUID: "abc", wantBacked: true},
		{name: "prefix with empty uid", configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix}, wantBacked: true},
		{name: "another providerID", configuration: map[string]any{"providerID": "azure:///subscriptions/x/vm"}},
		{name: "prefix not at start", configuration: map[string]any{"providerID": "x" + v1alpha3.AllocationProviderIDPrefix + "abc"}},
		{name: "non-string providerID", configuration: map[string]any{"providerID": 42}},
		{name: "nil configuration"},
		{name: "no providerID", configuration: map[string]any{"maxPods": 110}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &AgentConfig{Kubelet: AgentKubeletConfig{Configuration: tc.configuration}}

			uid, backed := AllocationUID(cfg)
			require.Equal(t, tc.wantUID, uid)
			require.Equal(t, tc.wantBacked, backed)
		})
	}

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()

		uid, backed := AllocationUID(nil)
		require.Empty(t, uid)
		require.False(t, backed)
	})
}
