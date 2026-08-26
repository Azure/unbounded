// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metrics

// DefaultDurationBuckets are histogram buckets suited for observing operation
// durations (e.g., reconciliation loops, network configuration). The range
// covers 5ms to 120s.
var DefaultDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// DefaultSizeBuckets are histogram buckets for observing byte sizes
// (e.g., HTTP response bodies, status push payloads). The range covers
// 256B to 10MB.
var DefaultSizeBuckets = []float64{
	256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 10485760,
}

// LabelController is the standard label key for identifying a controller.
const LabelController = "controller"

// LabelResult is the standard label key for operation outcomes.
const LabelResult = "result"
