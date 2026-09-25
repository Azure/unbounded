// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestNodeSelector(t *testing.T) {
	tests := []struct {
		name string
		pool string
		want map[string]string
	}{
		{
			name: "classic default",
			want: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"},
		},
		{
			name: "dedicated pool",
			pool: "stream",
			want: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", "agentpool": "stream"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := benchmarkConfig{ImagePlatform: "linux/amd64", NodePool: test.pool}
			if got := config.nodeSelector(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("nodeSelector() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateGantryStatus(t *testing.T) {
	status := daemonSetStatus{}
	status.Status.DesiredNumberScheduled = 1000
	status.Status.UpdatedNumberScheduled = 1000
	status.Status.NumberReady = 1000
	status.Status.NumberAvailable = 1000

	if err := validateGantryStatus(status, 1000); err != nil {
		t.Fatalf("validateGantryStatus: %v", err)
	}

	if err := validateGantryStatus(status, 300); err == nil {
		t.Fatalf("validateGantryStatus unexpectedly accepted an obsolete node count")
	}
}

func TestValidateBenchmarkDaemonSetStatus(t *testing.T) {
	status := daemonSetStatus{}
	status.Status.DesiredNumberScheduled = 1000
	status.Status.NumberReady = 1000

	if err := validateBenchmarkDaemonSetStatus(status, "restore", 1000); err != nil {
		t.Fatalf("validateBenchmarkDaemonSetStatus: %v", err)
	}

	if err := validateBenchmarkDaemonSetStatus(status, "restore", 300); err == nil {
		t.Fatalf("validateBenchmarkDaemonSetStatus unexpectedly accepted an obsolete node count")
	}
}
