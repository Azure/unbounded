// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

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