// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import "testing"

func TestCenterPivotCRDValidation(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, "unbounded-cloud.io_sites.yaml")

	tests := []struct {
		name        string
		centerPivot string
		want        []string
	}{
		{
			name: "enabled with two apps",
			centerPivot: `
      enabled: true
      nebraskaURL: https://nebraska.example.com/v1/update
      apps:
        - name: acl-nvidia
          appID: 11111111-2222-3333-4444-555555555555
          defaultTrack: stable
        - name: acl-amd
          appID: 99999999-2222-3333-4444-555555555555`,
		},
		{
			name:        "disabled needs nothing",
			centerPivot: `{enabled: false}`,
		},
		{
			name:        "enabled without apps",
			centerPivot: `{enabled: true, nebraskaURL: "https://nebraska.example.com/v1/update"}`,
			want:        []string{"nebraskaURL and apps are required when centerPivot is enabled"},
		},
		{
			name:        "enabled without a URL",
			centerPivot: `{enabled: true, apps: [{name: acl, appID: app}]}`,
			want:        []string{"nebraskaURL and apps are required when centerPivot is enabled"},
		},
		{
			name:        "an http URL",
			centerPivot: `{nebraskaURL: "http://nebraska.example.com/v1/update"}`,
			want:        []string{"spec.components.centerPivot.nebraskaURL"},
		},
		{
			name:        "two apps with one name",
			centerPivot: `{apps: [{name: acl, appID: a}, {name: acl, appID: b}]}`,
			want:        []string{"spec.components.centerPivot.apps[1]", "Duplicate value"},
		},
		{
			name:        "the default deadlines fit the default update timeout",
			centerPivot: `{enabled: true, nebraskaURL: "https://nebraska.example.com/v1/update", apps: [{name: acl, appID: app}]}`,
		},
		{
			name:        "an hour, with the default deadlines",
			centerPivot: `{enabled: true, nebraskaURL: "https://nebraska.example.com/v1/update", apps: [{name: acl, appID: app}], updateTimeout: 1h}`,
			want:        []string{"updateTimeout must fit staging"},
		},
		{
			name: "an hour, with deadlines that fit it",
			centerPivot: `{enabled: true, nebraskaURL: "https://nebraska.example.com/v1/update", apps: [{name: acl, appID: app}],
        updateTimeout: 1h, drainDeadline: 5m, restartDeadline: 5m}`,
		},
		{
			name:        "disabled isn't checked",
			centerPivot: `{enabled: false, updateTimeout: 1h}`,
		},
		{
			name:        "an app without an ID",
			centerPivot: `{apps: [{name: acl}]}`,
			want:        []string{"spec.components.centerPivot.apps[0].appID"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obj := parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: edge
spec:
  nodeCidrs: [10.0.0.0/16]
  podCidrAssignments:
    - cidrBlocks: [10.244.0.0/16]
  components:
    centerPivot: `+tt.centerPivot)

			assertErrors(t, v.validateCreate(obj), tt.want)
		})
	}
}
