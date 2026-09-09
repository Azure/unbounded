// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type nodeSelectionRunner struct {
	nodes []byte
}

func (runner nodeSelectionRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	return runner.nodes, nil
}

func TestBenchmarkWorkerSelection(t *testing.T) {
	runner := nodeSelectionRunner{nodes: []byte(`{"items":[
		{"metadata":{"name":"system","labels":{"kubernetes.io/os":"linux","kubernetes.io/arch":"amd64","gantry-benchmark":"system"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"worker","labels":{"kubernetes.io/os":"linux","kubernetes.io/arch":"amd64","gantry-benchmark":"worker"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"pending","labels":{"kubernetes.io/os":"linux","kubernetes.io/arch":"amd64","gantry-benchmark":"worker"}},"status":{"conditions":[{"type":"Ready","status":"False"}]}}
	]}`)}
	bench := &benchmark{config: benchmarkConfig{ImagePlatform: "linux/amd64", NodeCount: 1, NodeLabel: "gantry-benchmark"}, commands: runner}

	nodes, err := bench.targetNodes(context.Background())
	if err != nil || !reflect.DeepEqual(nodes, []string{"worker"}) {
		t.Fatalf("worker selection = %v, %v", nodes, err)
	}

	if bench.config.nodeSelector()["gantry-benchmark"] != "worker" {
		t.Fatal("workload selector does not require worker label")
	}

	bench.config.NodeCount = 2
	if _, err := bench.targetNodes(context.Background()); err == nil {
		t.Fatal("accepted missing Ready worker")
	}

	bench.config.NodeLabel = ""

	nodes, err = bench.targetNodes(context.Background())
	if err != nil || len(nodes) != 2 {
		t.Fatalf("legacy selection = %v, %v", nodes, err)
	}
}

func TestBenchmarkNodeLabelStateRoundTrip(t *testing.T) {
	state := benchmarkState{NodeLabel: "gantry-benchmark", NodeCount: 4997}

	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	var decoded benchmarkState
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.NodeLabel != state.NodeLabel || decoded.NodeCount != state.NodeCount {
		t.Fatalf("node selection changed after state round trip: %+v", decoded)
	}
}
