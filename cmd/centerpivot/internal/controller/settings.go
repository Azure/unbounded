// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/drain"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
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

// grantMargin is how long before Nebraska's update timeout every grant must
// have ended.
const grantMargin = 10 * time.Minute

// App is a Nebraska application a Site's nodes may run.
type App struct {
	Name         string
	AppID        string
	DefaultTrack string
}

// Settings are one Site's centerPivot settings, with defaults filled in.
type Settings struct {
	Server      string
	Apps        []App
	CAConfigMap string

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
	if spec == nil || !unboundedv1alpha3.ComponentEnabled(&spec.SiteComponentSpec) || spec.NebraskaURL == "" || len(spec.Apps) == 0 {
		return Settings{}, false
	}

	settings := Settings{
		Server:           spec.NebraskaURL,
		CAConfigMap:      spec.CAConfigMap,
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

	for _, app := range spec.Apps {
		settings.Apps = append(settings.Apps, App{Name: app.Name, AppID: app.AppID, DefaultTrack: app.DefaultTrack})
	}

	return settings, true
}

// AppFor returns the app a node with these labels runs: the one its app label
// names, or the Site's only app.
func (s Settings) AppFor(labels map[string]string) (App, error) {
	name, labeled := labels[names.AppLabel]
	if !labeled {
		if len(s.Apps) == 1 {
			return s.Apps[0], nil
		}

		return App{}, fmt.Errorf("the node has no %s label, and the Site has %d apps", names.AppLabel, len(s.Apps))
	}

	for _, app := range s.Apps {
		if app.Name == name {
			return app, nil
		}
	}

	return App{}, fmt.Errorf("the Site has no app %q, which the node's %s label names", name, names.AppLabel)
}

// Slots returns how many of managed nodes may be unavailable at once. A
// percentage other than 0% gives at least one, so a small Site's nodes still
// update.
func (s Settings) Slots(managed int) int {
	if s.MaxUnavailable.Type == intstr.Int {
		return max(0, s.MaxUnavailable.IntValue())
	}

	percent, err := intstr.GetScaledValueFromIntOrPercent(&s.MaxUnavailable, 100, false)
	if err != nil || percent <= 0 {
		return 0
	}

	slots, err := intstr.GetScaledValueFromIntOrPercent(&s.MaxUnavailable, managed, false)
	if err != nil {
		return 0
	}

	return max(1, slots)
}

// StageDeadline is how long the agent has to stage a version: its own timeout
// for the download, and the status deadline for the rest.
func (s Settings) StageDeadline() time.Duration {
	return names.AgentStageTimeout + s.StatusDeadline
}

// FinalizeDeadline is how long the agent has to finalize: its own timeout,
// and the status deadline for the rest.
func (s Settings) FinalizeDeadline() time.Duration {
	return names.AgentFinalizeTimeout + s.StatusDeadline
}

// Validate checks that every update can end its grant in time: staging, the
// drain, finalizing, and the restart, each to its deadline, and grantMargin,
// must fit in the update timeout (designs/centerpivot.md, section 4.8). The
// Site's CRD checks the same.
func (s Settings) Validate() error {
	if _, err := intstr.GetScaledValueFromIntOrPercent(&s.MaxUnavailable, 100, false); err != nil {
		return fmt.Errorf("maxUnavailable: %w", err)
	}

	if need := s.StageDeadline() + s.DrainDeadline + s.FinalizeDeadline() + s.RestartDeadline + grantMargin; need > s.UpdateTimeout {
		return fmt.Errorf("updateTimeout %s is shorter than the %s an update may hold its grant, with %s to spare", s.UpdateTimeout, need-grantMargin, grantMargin)
	}

	return nil
}

// TimeLeftToFinalize reports whether a grant from grantedAt still has time
// for the finalize and the restart, each to its deadline, with grantMargin to
// spare (designs/centerpivot.md, section 4.8).
func (s Settings) TimeLeftToFinalize(grantedAt, now time.Time) bool {
	return now.Sub(grantedAt)+s.FinalizeDeadline()+s.RestartDeadline <= s.UpdateTimeout-grantMargin
}

func duration(value *metav1.Duration, fallback time.Duration) time.Duration {
	if value == nil || value.Duration <= 0 {
		return fallback
	}

	return value.Duration
}
