// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestRolloutLimit(t *testing.T) {
	tests := map[string]struct {
		rollout *unboundedv1alpha3.MachineOperationRollout
		targets int
		want    int
	}{
		"no rollout": {
			targets: 5,
			want:    1,
		},
		"no max concurrent": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{},
			targets: 5,
			want:    1,
		},
		"integer": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromInt(3)),
			},
			targets: 5,
			want:    3,
		},
		"one hundred percent": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromString("100%")),
			},
			targets: 5,
			want:    5,
		},
		"fifty percent rounds up": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromString("50%")),
			},
			targets: 5,
			want:    3,
		},
		"one percent is at least one": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromString("1%")),
			},
			targets: 5,
			want:    1,
		},
		"invalid percentage falls back to one": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromString("many%")),
			},
			targets: 5,
			want:    1,
		},
		"zero falls back to one": {
			rollout: &unboundedv1alpha3.MachineOperationRollout{
				MaxConcurrent: intOrString(intstr.FromInt(0)),
			},
			targets: 5,
			want:    1,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, test.want, rolloutLimit(test.rollout, test.targets))
		})
	}
}

func TestChildName(t *testing.T) {
	operation := func(name, uid string) *unboundedv1alpha3.MachineOperation {
		return &unboundedv1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
	}
	suffix := regexp.MustCompile(`-[0-9a-f]{10}$`)

	t.Run("readable part and a stable suffix", func(t *testing.T) {
		parent := operation("reboot-rack", "uid-1")

		name := childName(parent, "machine-1")

		require.True(t, strings.HasPrefix(name, "reboot-rack-machine-1-"))
		require.Regexp(t, suffix, name)
		require.Equal(t, name, childName(parent, "machine-1"))
	})

	// "reboot" on "worker-01" and "reboot-worker" on "01" read the same.
	t.Run("pairs that read the same get different names", func(t *testing.T) {
		require.NotEqual(t,
			childName(operation("reboot", "uid-1"), "worker-01"),
			childName(operation("reboot-worker", "uid-2"), "01"))
	})

	// The children of a deleted parent can outlive it for a moment.
	t.Run("a parent created again under the same name gets new names", func(t *testing.T) {
		require.NotEqual(t,
			childName(operation("reboot", "uid-1"), "worker-01"),
			childName(operation("reboot", "uid-2"), "worker-01"))
	})

	t.Run("long names are cut to fit", func(t *testing.T) {
		parent := operation(strings.Repeat("p", 240), "uid-1")

		first := childName(parent, strings.Repeat("a", 20))
		second := childName(parent, strings.Repeat("b", 20))

		require.Len(t, first, maxChildNameLength)
		require.Regexp(t, suffix, first)
		require.NotEqual(t, first, second)
	})

	// A cut that lands after "." or "-" would leave a segment ending in it,
	// which is not a valid name and could never be created.
	t.Run("a cut never leaves a segment ending in a dot or a hyphen", func(t *testing.T) {
		limit := maxChildNameLength - childNameHashSize - 1

		for _, separator := range []string{".", "-", ".-."} {
			parent := operation(strings.Repeat("p", limit-len(separator))+separator+"tail", "uid-1")

			name := childName(parent, "machine-1")

			require.LessOrEqual(t, len(name), maxChildNameLength)
			require.Regexp(t, regexp.MustCompile(`p-[0-9a-f]{10}$`), name)
			require.Empty(t, validation.IsDNS1123Subdomain(name), name)
		}
	})
}
