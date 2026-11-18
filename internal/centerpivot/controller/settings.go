// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/centerpivot/drain"
)

// The defaults of the Site's centerPivot settings.
const (
	DefaultCheckInterval    = time.Hour
	DefaultUpdateTimeout    = 2 * time.Hour
	DefaultStatusDeadline   = 5 * time.Minute
	DefaultDrainDeadline    = 30 * time.Minute
	DefaultRestartDeadline  = 30 * time.Minute
	DefaultRecoveryDeadline = 30 * time.Minute
	DefaultSettleTime       = 5 * time.Minute
)

// Settings are one Site's centerPivot settings, with defaults filled in.
type Settings struct {
	Server       string
	AppID        string
	CAConfigMap  string
	DefaultTrack string

	MaxUnavailable   intstr.IntOrString
	CheckInterval    time.Duration
	UpdateTimeout    time.Duration
	StatusDeadline   time.Duration
	DrainDeadline    time.Duration
	RestartDeadline  time.Duration
	RecoveryDeadline time.Duration
	SettleTime       time.Duration

	Drain drain.Options
}

// SettingsFor returns the Site's settings. It reports false when the Site
// doesn't enable centerpivot, or doesn't name a Nebraska URL and app ID.
func SettingsFor(site *unboundedv1alpha3.Site) (Settings, bool) {
	spec := site.Spec.Components.CenterPivot
	if spec == nil || !unboundedv1alpha3.ComponentEnabled(&spec.SiteComponentSpec) || spec.NebraskaURL == "" || spec.AppID == "" {
		return Settings{}, false
	}

	settings := Settings{
		Server:           spec.NebraskaURL,
		AppID:            spec.AppID,
		CAConfigMap:      spec.CAConfigMap,
		DefaultTrack:     spec.DefaultTrack,
		MaxUnavailable:   intstr.FromInt32(1),
		CheckInterval:    duration(spec.CheckInterval, DefaultCheckInterval),
		UpdateTimeout:    duration(spec.UpdateTimeout, DefaultUpdateTimeout),
		StatusDeadline:   duration(spec.StatusDeadline, DefaultStatusDeadline),
		DrainDeadline:    duration(spec.DrainDeadline, DefaultDrainDeadline),
		RestartDeadline:  duration(spec.RestartDeadline, DefaultRestartDeadline),
		RecoveryDeadline: duration(spec.RecoveryDeadline, DefaultRecoveryDeadline),
		SettleTime:       duration(spec.SettleTime, DefaultSettleTime),
		Drain: drain.Options{
			DeleteEmptyDirData: spec.DeleteEmptyDirData,
			Force:              spec.MovePodsWithNoController,
		},
	}
	if spec.MaxUnavailable != nil {
		settings.MaxUnavailable = *spec.MaxUnavailable
	}

	return settings, true
}

// Slots returns how many of managed nodes may be unavailable at once. A
// percentage gives at least one, so a small Site's nodes still update.
func (s Settings) Slots(managed int) int {
	if s.MaxUnavailable.Type == intstr.Int {
		return max(0, s.MaxUnavailable.IntValue())
	}

	slots, err := intstr.GetScaledValueFromIntOrPercent(&s.MaxUnavailable, managed, false)
	if err != nil || slots < 1 {
		return 1
	}

	return slots
}

// TimeLeftToRestart reports whether a grant from grantedAt still has time to
// restart before Nebraska's update timeout, with 10 minutes to spare
// (designs/centerpivot.md, section 4.8).
func (s Settings) TimeLeftToRestart(grantedAt, now time.Time) bool {
	return now.Sub(grantedAt)+s.RestartDeadline <= s.UpdateTimeout-10*time.Minute
}

func duration(value *metav1.Duration, fallback time.Duration) time.Duration {
	if value == nil || value.Duration <= 0 {
		return fallback
	}

	return value.Duration
}
