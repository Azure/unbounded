// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestSettingsFor(t *testing.T) {
	t.Parallel()

	settings, ok := SettingsFor(testSite("edge", nil))
	assert.True(t, ok)
	assert.Equal(t, DefaultCheckInterval, settings.CheckInterval)
	assert.Equal(t, DefaultUpdateTimeout, settings.UpdateTimeout)
	assert.Equal(t, 1, settings.Slots(10), "one slot by default")
	assert.Equal(t, "stable", settings.DefaultTrack)

	site := testSite("edge", nil)
	site.Spec.Components.CenterPivot.CheckInterval = &metav1.Duration{Duration: 10 * time.Minute}
	site.Spec.Components.CenterPivot.MovePodsWithNoController = true
	settings, _ = SettingsFor(site)
	assert.Equal(t, 10*time.Minute, settings.CheckInterval)
	assert.True(t, settings.Drain.Force)

	for name, spec := range map[string]*unboundedv1alpha3.CenterPivotComponentSpec{
		"not set":     nil,
		"not enabled": {NebraskaURL: "https://nebraska.example.com/v1/update", AppID: "app"},
		"no URL":      {SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)}, AppID: "app"},
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
	} {
		assert.Equal(t, tt.want, Settings{MaxUnavailable: tt.maxUnavailable}.Slots(tt.managed), "%s of %d", tt.maxUnavailable.String(), tt.managed)
	}
}

func TestTimeLeftToRestart(t *testing.T) {
	t.Parallel()

	settings := Settings{UpdateTimeout: 2 * time.Hour, RestartDeadline: 30 * time.Minute}
	granted := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	assert.True(t, settings.TimeLeftToRestart(granted, granted.Add(80*time.Minute)), "30 minutes to restart, and 10 to spare")
	assert.False(t, settings.TimeLeftToRestart(granted, granted.Add(81*time.Minute)))
}
