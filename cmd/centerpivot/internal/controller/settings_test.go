// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
)

func TestSettingsFor(t *testing.T) {
	t.Parallel()

	settings, ok := SettingsFor(testSite("edge", nil))
	assert.True(t, ok)
	assert.Equal(t, DefaultCheckInterval, settings.CheckInterval)
	assert.Equal(t, DefaultUpdateTimeout, settings.UpdateTimeout)
	assert.Equal(t, 1, settings.Slots(10), "one slot by default")
	assert.Equal(t, []App{{Name: "acl", AppID: aclApp, DefaultTrack: "stable"}}, settings.Apps)

	site := testSite("edge", nil)
	site.Spec.Components.CenterPivot.CheckInterval = &metav1.Duration{Duration: 10 * time.Minute}
	site.Spec.Components.CenterPivot.MovePodsWithNoController = true
	settings, _ = SettingsFor(site)
	assert.Equal(t, 10*time.Minute, settings.CheckInterval)
	assert.True(t, settings.Drain.Force)

	for name, spec := range map[string]*unboundedv1alpha3.CenterPivotComponentSpec{
		"not set":     nil,
		"not enabled": {NebraskaURL: "https://nebraska.example.com/v1/update", Apps: []unboundedv1alpha3.CenterPivotApp{{Name: "acl", AppID: aclApp}}},
		"no URL":      {SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)}, Apps: []unboundedv1alpha3.CenterPivotApp{{Name: "acl", AppID: aclApp}}},
		"no apps":     {SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)}, NebraskaURL: "https://nebraska.example.com/v1/update"},
	} {
		site := testSite("edge", nil)
		site.Spec.Components.CenterPivot = spec
		_, ok := SettingsFor(site)
		assert.False(t, ok, name)
	}
}

func TestSlots(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		maxUnavailable intstr.IntOrString
		managed, want  int
	}{
		{maxUnavailable: intstr.FromInt32(3), managed: 10, want: 3},
		{maxUnavailable: intstr.FromInt32(0), managed: 10, want: 0},
		{maxUnavailable: intstr.FromString("25%"), managed: 10, want: 2},
		{maxUnavailable: intstr.FromString("25%"), managed: 2, want: 1},
		{maxUnavailable: intstr.FromString("0%"), managed: 10, want: 0},
	} {
		assert.Equal(t, tt.want, Settings{MaxUnavailable: tt.maxUnavailable}.Slots(tt.managed), "%s of %d", tt.maxUnavailable.String(), tt.managed)
	}
}

func TestTimeLeftToFinalize(t *testing.T) {
	t.Parallel()

	settings, _ := SettingsFor(testSite("edge", nil))

	assert.True(t, settings.TimeLeftToFinalize(start, start.Add(65*time.Minute)), "15 minutes to finalize, 30 to restart, and 10 to spare")
	assert.False(t, settings.TimeLeftToFinalize(start, start.Add(66*time.Minute)))
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		change  func(*unboundedv1alpha3.CenterPivotComponentSpec)
		wantErr string
	}{
		{name: "the defaults fit", change: func(*unboundedv1alpha3.CenterPivotComponentSpec) {}},
		{
			name: "deadlines shortened to fit an hour",
			change: func(spec *unboundedv1alpha3.CenterPivotComponentSpec) {
				spec.UpdateTimeout = &metav1.Duration{Duration: time.Hour}
				spec.DrainDeadline = &metav1.Duration{Duration: 5 * time.Minute}
				spec.RestartDeadline = &metav1.Duration{Duration: 5 * time.Minute}
			},
		},
		{
			name: "an hour, with the default deadlines",
			change: func(spec *unboundedv1alpha3.CenterPivotComponentSpec) {
				spec.UpdateTimeout = &metav1.Duration{Duration: time.Hour}
			},
			wantErr: "updateTimeout 1h0m0s is shorter than the 1h40m0s an update may hold its grant",
		},
		{
			name: "a percentage that isn't one",
			change: func(spec *unboundedv1alpha3.CenterPivotComponentSpec) {
				spec.MaxUnavailable = ptr.To(intstr.FromString("half"))
			},
			wantErr: "maxUnavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			site := testSite("edge", nil)
			tt.change(site.Spec.Components.CenterPivot)

			settings, ok := SettingsFor(site)
			require.True(t, ok)

			err := settings.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestAppFor(t *testing.T) {
	t.Parallel()

	acl := App{Name: "acl", AppID: aclApp}
	nvidia := App{Name: "acl-nvidia", AppID: nvidiaApp}

	for _, tt := range []struct {
		name    string
		apps    []App
		label   string
		want    App
		wantErr bool
	}{
		{name: "the only app, without a label", apps: []App{acl}, want: acl},
		{name: "the app the label names", apps: []App{acl, nvidia}, label: "acl-nvidia", want: nvidia},
		{name: "no label, and several apps", apps: []App{acl, nvidia}, wantErr: true},
		{name: "an app the Site doesn't have", apps: []App{acl}, label: "acl-amd", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			labels := map[string]string{}
			if tt.label != "" {
				labels[names.AppLabel] = tt.label
			}

			got, err := Settings{Apps: tt.apps}.AppFor(labels)
			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
