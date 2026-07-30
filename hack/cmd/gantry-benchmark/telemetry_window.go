// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

const (
	azureMetricInterval      = time.Minute
	azureMetricTrailingGuard = 3 * time.Minute
)

func nextMetricBoundary(now time.Time) time.Time {
	return now.UTC().Truncate(azureMetricInterval).Add(azureMetricInterval)
}

func metricWindowCloseBoundary(now time.Time) time.Time {
	return nextMetricBoundary(now).Add(azureMetricTrailingGuard)
}

func waitUntil(ctx context.Context, target time.Time) error {
	delay := time.Until(target)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *benchmark) beginTelemetryWindow(ctx context.Context, phase proxyPhase) (time.Time, error) {
	if !b.config.AzureTelemetry {
		return time.Time{}, nil
	}

	boundary := nextMetricBoundary(time.Now())
	writeAll(b.stdout, fmt.Sprintf("waiting for %s Azure metric window at %s\n", phase, boundary.Format(time.RFC3339)))

	if err := waitUntil(ctx, boundary); err != nil {
		return time.Time{}, err
	}

	return boundary, nil
}

func (b *benchmark) finishTelemetryWindow(ctx context.Context, phase proxyPhase) (time.Time, error) {
	if !b.config.AzureTelemetry {
		return time.Time{}, nil
	}

	boundary := metricWindowCloseBoundary(time.Now())
	writeAll(b.stdout, fmt.Sprintf("closing %s Azure metric window at %s\n", phase, boundary.Format(time.RFC3339)))

	if err := waitUntil(ctx, boundary); err != nil {
		return time.Time{}, err
	}

	return boundary, nil
}

func (b *benchmark) collectAzurePhaseOnce(
	ctx context.Context,
	phase phaseResult,
) (azurePhaseMeasurement, error) {
	acr, err := b.collectACRPulls(ctx, phase.Image, phase.Azure.Window)
	if err != nil {
		return azurePhaseMeasurement{}, err
	}

	privateEndpoint, err := b.collectPrivateEndpointBytes(ctx, phase.Azure.Window)
	if err != nil {
		return azurePhaseMeasurement{}, err
	}

	audit, err := b.collectAuditLatency(ctx, phase.Job, phase.Azure.Window)
	if err != nil {
		return azurePhaseMeasurement{}, err
	}

	return azurePhaseMeasurement{
		Window:          phase.Azure.Window,
		ACR:             acr,
		PrivateEndpoint: privateEndpoint,
		Audit:           audit,
		Complete:        acr.Complete && privateEndpoint.Complete && audit.Complete,
	}, nil
}

func (b *benchmark) collectAzurePhaseUntilStable(
	ctx context.Context,
	phase phaseResult,
) (azurePhaseMeasurement, error) {
	if !b.config.AzureTelemetry {
		return azurePhaseMeasurement{}, nil
	}

	pollContext, cancel := context.WithTimeout(ctx, b.config.TelemetryTimeout)
	defer cancel()

	stableWindow := max(2*b.config.TelemetryPollInterval, 30*time.Second)
	tracker := azureTelemetrySettlement{window: stableWindow}

	var (
		last    azurePhaseMeasurement
		lastErr error
	)

	for {
		measurement, err := b.collectAzurePhaseOnce(pollContext, phase)
		if err == nil {
			last = measurement
			lastErr = nil

			if tracker.Observe(time.Now(), measurement) {
				return measurement, nil
			}
		} else {
			lastErr = err
		}

		select {
		case <-pollContext.Done():
			if lastErr != nil {
				return azurePhaseMeasurement{}, fmt.Errorf("azure telemetry did not become queryable: %w", lastErr)
			}

			return azurePhaseMeasurement{}, fmt.Errorf(
				"azure telemetry did not become complete and stable before %s: %+v",
				b.config.TelemetryTimeout,
				last,
			)
		case <-time.After(b.config.TelemetryPollInterval):
		}
	}
}

type azureTelemetrySettlement struct {
	window      time.Duration
	stableSince time.Time
	last        azurePhaseMeasurement
	initialized bool
}

func (s *azureTelemetrySettlement) Observe(now time.Time, current azurePhaseMeasurement) bool {
	if !current.Complete {
		s.initialized = false
		s.stableSince = time.Time{}

		return false
	}

	if !s.initialized || !reflect.DeepEqual(s.last, current) {
		s.last = current
		s.stableSince = now
		s.initialized = true

		return false
	}

	return now.Sub(s.stableSince) >= s.window
}
