// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const metricsCollectionTimeout = 5 * time.Second

var gpuTflopsMetricPattern = regexp.MustCompile(`^gpu([0-7])MinTflops$`)

type CertificationMetricsCollector struct {
	reader              client.Reader
	r07MinimumAssertion *prometheus.Desc
}

func NewCertificationMetricsCollector(reader client.Reader) *CertificationMetricsCollector {
	return &CertificationMetricsCollector{
		reader: reader,
		r07MinimumAssertion: prometheus.NewDesc(
			"project_signal_fleet_certification_r07_assertion_decimal",
			"Minimum BF16 throughput projected from immutable completed R07 stage evidence.",
			[]string{"namespace", "run", "run_uid", "assertion"},
			nil,
		),
	}
}

func (c *CertificationMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.r07MinimumAssertion
}

func (c *CertificationMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectionTimeout)
	defer cancel()

	var runs signalv1.CertificationRunList
	if err := c.reader.List(ctx, &runs); err != nil {
		ch <- prometheus.NewInvalidMetric(
			c.r07MinimumAssertion,
			fmt.Errorf("list CertificationRuns: %w", err),
		)

		return
	}

	for index := range runs.Items {
		c.collectRun(&runs.Items[index], ch)
	}
}

func (c *CertificationMetricsCollector) collectRun(
	run *signalv1.CertificationRun,
	ch chan<- prometheus.Metric,
) {
	if run.Status.Evidence.Identity == nil || len(run.Spec.Participants) == 0 {
		return
	}

	for stageIndex := range run.Status.Evidence.Identity.Stages {
		stage := &run.Status.Evidence.Identity.Stages[stageIndex]
		if stage.Name != "r07-gpu-gemm-bf16" {
			continue
		}

		minimum := 0.0
		hasMinimum := false

		for measurementIndex := range stage.Measurements {
			measurement := &stage.Measurements[measurementIndex]
			matches := gpuTflopsMetricPattern.FindStringSubmatch(measurement.Name)
			if len(matches) != 2 || measurement.Unit != "TFLOP/s" {
				continue
			}

			value, ok := numericMetricValue(measurement.Value)
			if !ok {
				continue
			}

			if !hasMinimum || value < minimum {
				minimum = value
				hasMinimum = true
			}
		}

		if hasMinimum {
			ch <- prometheus.MustNewConstMetric(
				c.r07MinimumAssertion,
				prometheus.GaugeValue,
				minimum,
				run.Namespace,
				run.Name,
				string(run.UID),
				"bf16-throughput",
			)
		}
	}
}

func numericMetricValue(value signalv1.TypedValue) (float64, bool) {
	switch value.Type {
	case signalv1.TypedValueTypeInteger:
		if value.IntegerValue == nil {
			return 0, false
		}

		return float64(*value.IntegerValue), true
	case signalv1.TypedValueTypeDecimal:
		if value.DecimalValue == nil {
			return 0, false
		}

		parsed, err := strconv.ParseFloat(*value.DecimalValue, 64)

		return parsed, err == nil
	default:
		return 0, false
	}
}
