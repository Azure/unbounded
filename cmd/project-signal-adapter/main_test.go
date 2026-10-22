// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/internal/signaladapter"
)

func TestWriteOutputsProjectsCleanupFailure(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	output := signaladapter.AggregateOutput{
		ExecutionState:  "Error",
		Verdict:         "Inconclusive",
		Reason:          "BackendExecutionFailed",
		CleanupFailure:  "BackendCleanupFailed",
		BackendEvidence: `{"version":"v1","complete":true,"participants":[{}]}`,
	}
	if err := writeOutputs(directory, output); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"reason":           "BackendExecutionFailed",
		"cleanup-failure":  "BackendCleanupFailed",
		"backend-evidence": `{"version":"v1","complete":true,"participants":[{}]}`,
	} {
		got, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestWriteOutputsRejectsOversizedCleanupFailure(t *testing.T) {
	t.Parallel()

	err := writeOutputs(t.TempDir(), signaladapter.AggregateOutput{
		CleanupFailure: string(make([]byte, signaladapter.MaxCleanupFailureBytes+1)),
	})
	if err == nil {
		t.Fatal("writeOutputs accepted oversized cleanup failure")
	}
}

func TestWriteOutputsRejectsMalformedBackendEvidence(t *testing.T) {
	t.Parallel()

	err := writeOutputs(t.TempDir(), signaladapter.AggregateOutput{
		BackendEvidence: `{"version":"v1","complete":true}`,
	})
	if err == nil {
		t.Fatal("writeOutputs accepted malformed backend evidence")
	}
}
