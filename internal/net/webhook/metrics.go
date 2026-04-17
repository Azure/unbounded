// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/Azure/unbounded-kube/internal/net/metrics"
)

const controllerNamespace = "unbounded_cni_controller"

var (
	webhookRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "webhook_requests_total",
		Help:      "Total webhook admission requests by resource, operation, and result.",
	}, []string{"resource", "operation", "result"})

	webhookRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: controllerNamespace,
		Name:      "webhook_request_duration_seconds",
		Help:      "Webhook admission request duration in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"resource", "operation"})
)
