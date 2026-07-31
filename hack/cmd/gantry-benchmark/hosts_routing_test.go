// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestRenderHosts(t *testing.T) {
	state := benchmarkState{
		RunID:                  "run-1",
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}

	baseline, err := renderHosts(state, hostsModeBaseline)
	if err != nil {
		t.Fatalf("render baseline: %v", err)
	}

	if !strings.Contains(baseline, `server = "https://baseline.azurecr.io"`) ||
		!strings.Contains(baseline, `[host."https://baseline.azurecr.io"]`) ||
		strings.Contains(baseline, "acr-origin-proxy") ||
		strings.Contains(baseline, "127.0.0.1") {
		t.Fatalf("unexpected baseline hosts.toml:\n%s", baseline)
	}

	gantry, err := renderHosts(state, hostsModeGantry)
	if err != nil {
		t.Fatalf("render Gantry: %v", err)
	}

	// STRICT mode: Gantry is the ONLY upstream, so there must be no `server=`
	// fall-through that would let containerd bypass Gantry to ACR.
	if strings.Contains(gantry, "server =") ||
		!strings.Contains(gantry, `[host."http://127.0.0.1:5000"]`) ||
		strings.Contains(gantry, "gantry.azurecr.io") ||
		strings.Contains(gantry, "skip_verify") {
		t.Fatalf("unexpected Gantry hosts.toml:\n%s", gantry)
	}
}

func TestNodeRoutingScriptsParse(t *testing.T) {
	runner := &captureApplyRunner{}
	benchmark := &benchmark{
		config:   benchmarkConfig{Namespace: "gantry-benchmark", NodeCount: 1},
		commands: runner,
	}

	state := benchmarkState{RunID: "run-1", ACRLoginServer: "bench.azurecr.io"}
	if err := benchmark.restoreHosts(context.Background(), state); err != nil {
		t.Fatalf("restoreHosts: %v", err)
	}

	var restorer map[string]any
	if err := json.Unmarshal(runner.applied[len(runner.applied)-1], &restorer); err != nil {
		t.Fatalf("decode applied restorer: %v", err)
	}

	tests := []struct {
		name      string
		daemonSet map[string]any
	}{
		{name: "restorer", daemonSet: restorer},
	}

	installer, err := benchmark.hostsInstallerDaemonSet(state, hostsModeBaseline)
	if err != nil {
		t.Fatalf("hostsInstallerDaemonSet: %v", err)
	}

	tests = append(tests, struct {
		name      string
		daemonSet map[string]any
	}{name: "installer", daemonSet: installer})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := daemonSetScript(t, test.daemonSet)
			command := exec.Command("sh", "-n")

			command.Stdin = bytes.NewBufferString(script)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("sh -n: %v\n%s", err, output)
			}
		})
	}
}

func TestRestoreHostsTargetsBothDirectRegistries(t *testing.T) {
	runner := &captureApplyRunner{}
	benchmark := &benchmark{
		config:   benchmarkConfig{Namespace: "gantry-benchmark", NodeCount: 1},
		commands: runner,
	}
	state := benchmarkState{
		RunID:                  "run-1",
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}

	if err := benchmark.restoreHosts(context.Background(), state); err != nil {
		t.Fatalf("restoreHosts: %v", err)
	}

	if len(runner.applied) != 2 {
		t.Fatalf("applied restorers = %d, want 2", len(runner.applied))
	}

	for index, registry := range []string{"baseline.azurecr.io", "gantry.azurecr.io"} {
		manifest := string(runner.applied[index])
		if !strings.Contains(manifest, registry) || !strings.Contains(manifest, `/host-state/${RUN_ID}/${REGISTRY_HOST}`) {
			t.Fatalf("restorer %d does not target %s with registry-scoped backup:\n%s", index, registry, manifest)
		}
	}
}

type captureApplyRunner struct {
	applied [][]byte
}

func (r *captureApplyRunner) Run(_ context.Context, stdin []byte, _ string, args ...string) ([]byte, error) {
	if len(stdin) != 0 {
		r.applied = append(r.applied, append([]byte(nil), stdin...))

		return nil, nil
	}

	for index := 0; index+1 < len(args); index++ {
		if args[index] == "get" && args[index+1] == "daemonset" {
			return []byte(`{"status":{"desiredNumberScheduled":1,"numberReady":1}}`), nil
		}
	}

	return nil, nil
}

func daemonSetScript(t *testing.T, daemonSet map[string]any) string {
	t.Helper()

	spec := requireMap(t, daemonSet["spec"], "spec")
	template := requireMap(t, spec["template"], "spec.template")
	podSpec := requireMap(t, template["spec"], "spec.template.spec")

	containers, ok := podSpec["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("unexpected containers value %T", podSpec["containers"])
	}

	container := requireMap(t, containers[0], "container")
	switch command := container["command"].(type) {
	case []string:
		return command[2]
	case []any:
		value, ok := command[2].(string)
		if !ok {
			t.Fatalf("unexpected command element type %T", command[2])
		}

		return value
	default:
		t.Fatalf("unexpected command type %T", command)

		return ""
	}
}

func requireMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()

	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s has type %T, want map[string]any", name, value)
	}

	return result
}
