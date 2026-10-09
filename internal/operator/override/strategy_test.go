// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateDaemonSetStrategy(t *testing.T) {
	for _, tc := range []struct {
		name, strategy, want string
	}{
		{"on-delete", "{type: OnDelete}", ""},
		{"rolling", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: '100%', maxSurge: 0}}", ""},
		{"rolling-only", "{rollingUpdate: {maxUnavailable: 1}}", ""},
		{"empty", "{}", ""},
		{"explicit-null", "{type: OnDelete, rollingUpdate: null}", "null"},
		{"empty-rolling", "{type: OnDelete, rollingUpdate: {}}", "rollingUpdate"},
		{"populated-rolling", "{type: OnDelete, rollingUpdate: {maxUnavailable: 1}}", "rollingUpdate"},
		{"directive", "{type: OnDelete, $patch: replace}", "directive"},
		{"unknown-type", "{type: Recreate}", "type"},
		{"malformed-type", "{type: [OnDelete]}", "type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFragment(t, "component: racer\nkind: DaemonSet\nname: racer-dataplane\npatch:\n  spec:\n    updateStrategy: "+tc.strategy+"\n")
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestNormalizeDaemonSetStrategyScope(t *testing.T) {
	for _, tc := range []struct {
		name, kind, strategyType string
		rolling, remove          bool
	}{
		{"daemonset-ondelete", "DaemonSet", "OnDelete", true, true},
		{"already-ondelete", "DaemonSet", "OnDelete", false, false},
		{"rolling-unchanged", "DaemonSet", "RollingUpdate", true, false},
		{"deployment-unchanged", "Deployment", "OnDelete", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strategy := map[string]any{"type": tc.strategyType}
			if tc.rolling {
				strategy["rollingUpdate"] = map[string]any{"maxUnavailable": int64(1)}
			}

			workload := &unstructured.Unstructured{Object: map[string]any{
				"kind": tc.kind, "spec": map[string]any{"updateStrategy": strategy},
			}}

			want := workload.DeepCopy()
			if tc.remove {
				unstructured.RemoveNestedField(want.Object, "spec", "updateStrategy", "rollingUpdate")
			}

			require.NoError(t, normalizeDaemonSetStrategy(workload, nil))
			require.Equal(t, want, workload)
			require.NoError(t, normalizeDaemonSetStrategy(workload, nil))
			require.Equal(t, want, workload, "normalization must be idempotent")
		})
	}
}
