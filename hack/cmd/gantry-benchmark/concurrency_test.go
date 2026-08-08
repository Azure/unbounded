// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"testing"
)

type recordedCommand struct {
	stdin []byte
	name  string
	args  []string
}

type recordingRunner struct {
	commands []recordedCommand
}

func (r *recordingRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{
		stdin: append([]byte(nil), stdin...),
		name:  name,
		args:  append([]string(nil), args...),
	})

	return nil, nil
}

func TestAcquireLockUsesAtomicCreate(t *testing.T) {
	runner := &recordingRunner{}
	benchmark := &benchmark{
		config:   benchmarkConfig{Namespace: "gantry-benchmark", GantryNamespace: "gantry-system"},
		commands: runner,
	}

	if err := benchmark.acquireLock(context.Background(), "run-1"); err != nil {
		t.Fatalf("acquireLock: %v", err)
	}

	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(runner.commands))
	}

	command := runner.commands[0]
	if command.name != "kubectl" || len(command.args) < 1 || command.args[0] != "create" {
		t.Fatalf("command = %s %v, want kubectl create", command.name, command.args)
	}

	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(command.stdin, &object); err != nil {
		t.Fatalf("decode lock object: %v", err)
	}

	if object.Metadata.Name != lockConfigMapName || object.Data["run-id"] != "run-1" {
		t.Fatalf("lock object = %+v", object)
	}
}

func TestPatchGantryConfigMapUsesCompareAndSwap(t *testing.T) {
	runner := &recordingRunner{}
	benchmark := &benchmark{
		config: benchmarkConfig{
			GantryNamespace: "gantry-system",
			GantryConfigMap: "gantry-config",
		},
		commands: runner,
	}

	if err := benchmark.patchGantryConfigMap(context.Background(), "original", "patched"); err != nil {
		t.Fatalf("patchGantryConfigMap: %v", err)
	}

	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(runner.commands))
	}

	command := runner.commands[0]
	if command.name != "kubectl" {
		t.Fatalf("command name = %q, want kubectl", command.name)
	}

	var (
		patchArgument string
		patchType     string
	)

	for index := 0; index+1 < len(command.args); index++ {
		switch command.args[index] {
		case "--patch":
			patchArgument = command.args[index+1]
		case "--type=json":
			patchType = command.args[index]
		}
	}

	if patchType != "--type=json" {
		t.Fatalf("patch type = %q, want --type=json", patchType)
	}

	var operations []struct {
		Operation string `json:"op"`
		Path      string `json:"path"`
		Value     string `json:"value"`
	}
	if err := json.Unmarshal([]byte(patchArgument), &operations); err != nil {
		t.Fatalf("decode JSON patch: %v", err)
	}

	if len(operations) != 2 ||
		operations[0].Operation != "test" || operations[0].Value != "original" ||
		operations[1].Operation != "replace" || operations[1].Value != "patched" {
		t.Fatalf("operations = %+v", operations)
	}
}
