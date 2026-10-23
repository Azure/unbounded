// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/racer/workload"
)

// dataplaneContainer returns the live dataplane container.
func dataplaneContainer(t *testing.T, env *component.Env) corev1.Container {
	t.Helper()

	for _, c := range liveDataplane(t, env).Spec.Template.Spec.Containers {
		if c.Name == workload.ContainerName {
			return c
		}
	}

	t.Fatal("dataplane container not found")

	return corev1.Container{}
}

func requireQuantity(t *testing.T, want string, list corev1.ResourceList, name corev1.ResourceName) {
	t.Helper()

	got, ok := list[name]
	require.True(t, ok, "%s missing from %v", name, list)

	wantQ := resource.MustParse(want)
	require.Zero(t, wantQ.Cmp(got), "%s = %s, want %s", name, got.String(), want)
}

// TestDataplaneDefaultResources pins the literal default so a changed
// default is caught, not just compared against its own constant.
func TestDataplaneDefaultResources(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	for pass := range 3 {
		if pass > 0 {
			persist(t, env, planPass(t, env))
		}

		c := dataplaneContainer(t, env)
		requireQuantity(t, "1Gi", c.Resources.Requests, corev1.ResourceMemory)
		require.Empty(t, c.Resources.Limits, "pass %d: dataplane must have no limits by default", pass)
	}
}

// applyOverride parses, validates, and applies an override document to a
// fresh plan, then persists it.
func applyOverride(t *testing.T, env *component.Env, doc string) {
	t.Helper()

	entries, problems, err := override.Parse(map[string]string{"racer.yaml": doc})
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, override.ValidateErr(entries))

	plan := planPass(t, env)
	report := override.Apply(plan, entries, nil)
	require.NoError(t, report.Err())
	require.False(t, report.Failed())
	persist(t, env, plan)
}

func resourceOverride(resources string) string {
	return `apiVersion: ` + override.APIVersion + `
overrides:
  - component: racer
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: dataplane
                resources:
` + resources
}

func TestDataplaneResourceOverrides(t *testing.T) {
	for name, tc := range map[string]struct {
		resources    string
		wantRequest  string
		wantLimit    string
		wantNoLimits bool
	}{
		"request": {
			resources: `                  requests:
                    memory: 2Gi
`,
			wantRequest:  "2Gi",
			wantNoLimits: true,
		},
		"limit keeps default request": {
			resources: `                  limits:
                    memory: 4Gi
`,
			wantRequest: "1Gi",
			wantLimit:   "4Gi",
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			before := dataplaneContainer(t, env)
			doc := resourceOverride(tc.resources)

			// Overrides are reapplied every pass; the result must be stable.
			for range 2 {
				applyOverride(t, env, doc)

				c := dataplaneContainer(t, env)
				requireQuantity(t, tc.wantRequest, c.Resources.Requests, corev1.ResourceMemory)

				if tc.wantNoLimits {
					require.Empty(t, c.Resources.Limits)
				} else {
					requireQuantity(t, tc.wantLimit, c.Resources.Limits, corev1.ResourceMemory)
				}

				// Other requests and the rest of the container are preserved.
				for res, q := range before.Resources.Requests {
					if res != corev1.ResourceMemory {
						requireQuantity(t, q.String(), c.Resources.Requests, res)
					}
				}

				require.Equal(t, before.Image, c.Image)
				require.Equal(t, before.Env, c.Env)
				require.Equal(t, before.EnvFrom, c.EnvFrom)
				require.Equal(t, before.VolumeMounts, c.VolumeMounts)
			}

			// Removing the override restores the default.
			persist(t, env, planPass(t, env))
			c := dataplaneContainer(t, env)
			requireQuantity(t, "1Gi", c.Resources.Requests, corev1.ResourceMemory)
			require.Empty(t, c.Resources.Limits)
		})
	}
}
