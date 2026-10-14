// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

var burnComponents = []string{"gpu-burn", "cpu-stress", "memory-integrity", "storage-integrity"}

func TestGroupedBurnRetainsConcurrentTestResults(t *testing.T) {
	t.Parallel()

	run := startGroupedBurn(t, nil)
	run.wait(t, false)

	assertFileContents(t, filepath.Join(run.results, "verdict"), "Pass")

	for _, component := range burnComponents {
		assertFileContents(t, filepath.Join(run.results, component+"-verdict"), "Pass")
		measurements, err := os.ReadFile(filepath.Join(run.results, component+"-measurements.txt"))
		require.NoError(t, err)
		require.NotEmpty(t, measurements)
	}

	devices, err := os.ReadFile(filepath.Join(run.results, "devices.txt"))
	require.NoError(t, err)
	require.Contains(t, string(devices), "GPU-test-0")
	require.FileExists(t, filepath.Join(run.work, "gpu-started"))
}

func TestGroupedBurnRejectsMissingCoverageAndHostFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		env    map[string]string
		reason string
	}{
		{"no GPU measurements", map[string]string{"TEST_GPU_MODE": "empty"}, "GPUBurnMeasurementsMissing"},
		{"partial GPU coverage", map[string]string{"EXPECTED_GPU_COUNT": "2", "OBSERVED_GPU_CAPACITY": "2", "OBSERVED_GPU_ALLOCATABLE": "2", "TEST_GPU_MODE": "partial"}, "GPUBurnMeasurementsMissing"},
		{"logged GPU computation failure", map[string]string{"TEST_GPU_MODE": "warning"}, "GPUBurnComputationFailed"},
		{"memory failure", map[string]string{"TEST_FAIL_COMPONENT": "memory-integrity"}, "HostTestExitedEarly"},
		{"inventory mismatch", map[string]string{"OBSERVED_GPU_CAPACITY": "0"}, "NodeGPUInventoryMismatch"},
		{"lost safety telemetry", map[string]string{"TEST_DMESG_FAIL_AFTER_GPU_START": "true"}, "XidTelemetryUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			run := startGroupedBurn(t, test.env)
			run.wait(t, false)
			assertFileContents(t, filepath.Join(run.results, "verdict"), "Fail")

			if test.env["TEST_FAIL_COMPONENT"] == "" {
				assertFileContents(t, filepath.Join(run.results, "reason"), test.reason)
			} else {
				reason, err := os.ReadFile(filepath.Join(run.results, "reason"))
				require.NoError(t, err)
				require.Contains(t, []string{"HostTestExitedEarly", "HostTestFailed"}, string(reason))
			}

			for _, component := range burnComponents {
				require.FileExists(t, filepath.Join(run.results, component+"-verdict"))
				require.FileExists(t, filepath.Join(run.results, component+"-measurements.txt"))
			}

			if test.env["TEST_FAIL_COMPONENT"] != "" {
				assertFileContents(t, filepath.Join(run.results, "memory-integrity-verdict"), "Fail")
			}
		})
	}
}

func TestGroupedBurnRejectsLateCPUFailureAfterGPUComputesSuccessfully(t *testing.T) {
	t.Parallel()

	run := startGroupedBurn(t, map[string]string{"TEST_FAIL_COMPONENT": "cpu-after-complete"})
	run.wait(t, false)
	assertFileContents(t, filepath.Join(run.results, "verdict"), "Fail")
	assertFileContents(t, filepath.Join(run.results, "cpu-stress-verdict"), "Fail")
	assertFileContents(t, filepath.Join(run.results, "gpu-burn-verdict"), "Pass")
	measurements, err := os.ReadFile(filepath.Join(run.results, "gpu-burn-measurements.txt"))
	require.NoError(t, err)
	require.Contains(t, string(measurements), "test=gpu-burn verdict=Pass reason=GPUBurnCompleted")
	require.Contains(t, string(measurements), "group_verdict=Fail")
}

func TestGroupedBurnCancellationStopsEveryComponent(t *testing.T) {
	t.Parallel()

	run := startGroupedBurn(t, map[string]string{
		"BURN_DURATION_SECONDS":   "30",
		"SAMPLE_INTERVAL_SECONDS": "60",
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(run.work, "gpu-started"))
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, run.commands["gpu-burn"].Process.Signal(syscall.SIGTERM))
	run.wait(t, true)
	assertFileContents(t, filepath.Join(run.results, "verdict"), "Inconclusive")
	assertFileContents(t, filepath.Join(run.results, "reason"), "Canceled")

	for _, component := range burnComponents {
		verdict, err := os.ReadFile(filepath.Join(run.results, component+"-verdict"))
		require.NoError(t, err)
		require.NotEqual(t, "Pass", string(verdict), component)
	}
}

func TestGroupedBurnBoundsCarriageReturnProgressEvidence(t *testing.T) {
	t.Parallel()

	run := startGroupedBurn(t, map[string]string{"TEST_GPU_MODE": "large"})
	run.wait(t, false)
	assertFileContents(t, filepath.Join(run.results, "verdict"), "Pass")
	measurements, err := os.ReadFile(filepath.Join(run.results, "gpu-burn-measurements.txt"))
	require.NoError(t, err)
	require.Less(t, len(measurements), 16*1024)
	require.Contains(t, string(measurements), "100 Gflop/s")
}

type groupedBurn struct {
	results  string
	work     string
	commands map[string]*exec.Cmd
	logs     map[string]*bytes.Buffer
}

func startGroupedBurn(t *testing.T, overrides map[string]string) *groupedBurn {
	t.Helper()

	dir := t.TempDir()
	run := &groupedBurn{
		results:  filepath.Join(dir, "results"),
		work:     filepath.Join(dir, "work"),
		commands: map[string]*exec.Cmd{},
		logs:     map[string]*bytes.Buffer{},
	}
	require.NoError(t, os.MkdirAll(run.results, 0o755))
	require.NoError(t, os.MkdirAll(run.work, 0o755))

	for name, script := range map[string]string{
		"nvidia-smi": `#!/usr/bin/env bash
for ((i=0; i<EXPECTED_GPU_COUNT; i++)); do
  if [[ "$*" == *"--query-gpu=index,uuid"* ]]; then
    printf '%s, GPU-test-%s, 0000:01:00.0, NVIDIA B200\n' "$i" "$i"
  else
    printf '%s, NVIDIA B200, 31, 57, 900, 1000, 0, 0, 0x0\n' "$i"
  fi
done
`,
		"dmesg": `#!/usr/bin/env bash
if [[ "${TEST_DMESG_FAIL_AFTER_GPU_START:-}" == true && -e "$SIGNAL_TEST_WORK/gpu-started" ]]; then
  exit 13
fi
printf '%s\n' 'kernel: healthy'
`,
		"dd": `#!/usr/bin/env bash
file=
for arg in "$@"; do
  case "$arg" in of=*) file="${arg#of=}" ;; esac
done
if [[ -z "$file" && "${TEST_FAIL_COMPONENT:-}" == cpu-after-complete ]]; then
  while [[ ! -e "$SIGNAL_TEST_WORK/burn-complete" ]]; do
    [[ ! -e "$SIGNAL_TEST_WORK/stop" ]] || exit 1
    sleep 0.01
  done
  exit 1
fi
sleep 0.01
if [[ -n "$file" ]]; then
  printf '%s\n' pattern > "$file"
else
  printf '%s\n' cpu-work
fi
`,
		"sha256sum": `#!/usr/bin/env bash
if [[ "${TEST_FAIL_COMPONENT:-}" == memory-integrity &&
      "$*" == *"/memory/"* && -e "$SIGNAL_TEST_WORK/gpu-started" ]]; then
  printf '%s\n' 'changed digest'
else
  cksum "$@"
fi
`,
		"gpu_burn": `#!/usr/bin/env bash
trap 'exit 143' TERM INT
for test in cpu-stress memory-integrity storage-integrity; do
  [[ -e "$SIGNAL_TEST_WORK/$test.started" ]] || exit 61
done
touch "$SIGNAL_TEST_WORK/gpu-started"
end=$((SECONDS + ${@: -1}))
while ((SECONDS < end)); do
  [[ ! -e "$SIGNAL_TEST_WORK/stop" ]] || exit 143
  sleep 0.1
done
if [[ "${TEST_GPU_MODE:-}" == large ]]; then
  head -c 131072 /dev/zero | tr '\0' x
  printf '\r'
fi
case "${TEST_GPU_MODE:-complete}" in
empty) exit 0 ;;
warning) printf '%s\n' "100.0% proc'd: 5 (100 Gflop/s) errors: 1 (WARNING!) temps: 31 C" ;;
*)
  count="$EXPECTED_GPU_COUNT"
  [[ "${TEST_GPU_MODE:-}" != partial ]] || count=1
  line="100.0% proc'd: "
  for ((i=0; i<count; i++)); do
    ((i == 0)) || line+=" - "
    line+="5 (100 Gflop/s)"
  done
  printf '%s\n' "$line  errors: 0  temps: 31 C"
  ;;
esac
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}

	env := map[string]string{
		"PATH":                            dir + ":" + os.Getenv("PATH"),
		"SIGNAL_TEST_WORK":                run.work,
		"EXPECTED_GPU_PRODUCT_REGEX":      "^NVIDIA B200$",
		"EXPECTED_GPU_COUNT":              "1",
		"OBSERVED_GPU_CAPACITY":           "1",
		"OBSERVED_GPU_ALLOCATABLE":        "1",
		"BURN_DURATION_SECONDS":           "2",
		"SAMPLE_INTERVAL_SECONDS":         "1",
		"POWER_LIMIT_TOLERANCE_PERCENT":   "5",
		"POWER_LIMIT_CONSECUTIVE_SAMPLES": "3",
		"CPU_WORKERS":                     "1",
		"MEMORY_MIB":                      "1",
		"STORAGE_MIB":                     "1",
	}
	for key, value := range overrides {
		env[key] = value
	}

	script := certificationScript(t, "gpu-burn-canary.sh")
	script = strings.ReplaceAll(script, "/results/", run.results+"/")
	script = strings.ReplaceAll(script, "/work/", run.work+"/")
	script = strings.ReplaceAll(script, "/dev/termination-log", filepath.Join(dir, "termination-log"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(run.work, "stop"), nil, 0o600); err != nil {
			t.Error(err)
		}

		cancel()

		for _, cmd := range run.commands {
			if cmd.ProcessState == nil {
				_ = cmd.Wait()
			}
		}
	})

	for _, component := range []string{"cpu-stress", "memory-integrity", "storage-integrity", "gpu-burn"} {
		cmd := exec.CommandContext(ctx, "bash", "-c", script, "signal-test", component)

		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}

		log := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = log, log
		run.commands[component], run.logs[component] = cmd, log
		require.NoError(t, cmd.Start())
	}

	return run
}

func (run *groupedBurn) wait(t *testing.T, canceled bool) {
	t.Helper()

	for _, component := range burnComponents {
		err := run.commands[component].Wait()
		if component == "gpu-burn" && canceled {
			require.Error(t, err)
		} else {
			require.NoError(t, err, "%s: %s", component, run.logs[component].String())
		}
	}
}

type workflowManifest struct {
	Spec struct {
		Arguments struct {
			Parameters []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"parameters"`
		} `yaml:"arguments"`
		Templates []struct {
			Name         string `yaml:"name"`
			PodSpecPatch string `yaml:"podSpecPatch"`
			Steps        [][]struct {
				Name     string `yaml:"name"`
				When     string `yaml:"when"`
				Template string `yaml:"template"`
			} `yaml:"steps"`
			Container struct {
				Command []string `yaml:"command"`
				Args    []string `yaml:"args"`
			} `yaml:"container"`
			Resource struct {
				Manifest          string `yaml:"manifest"`
				SuccessCondition  string `yaml:"successCondition"`
				FailureCondition  string `yaml:"failureCondition"`
				SetOwnerReference bool   `yaml:"setOwnerReference"`
			} `yaml:"resource"`
			ContainerSet struct {
				Containers []struct {
					Name         string   `yaml:"name"`
					Args         []string `yaml:"args"`
					Dependencies []string `yaml:"dependencies"`
					Resources    struct {
						Requests map[string]string `yaml:"requests"`
						Limits   map[string]string `yaml:"limits"`
					} `yaml:"resources"`
				} `yaml:"containers"`
			} `yaml:"containerSet"`
			Outputs struct {
				Parameters []struct {
					Name      string `yaml:"name"`
					Value     string `yaml:"value"`
					ValueFrom struct {
						Expression string `yaml:"expression"`
						Parameter  string `yaml:"parameter"`
						Path       string `yaml:"path"`
					} `yaml:"valueFrom"`
				} `yaml:"parameters"`
			} `yaml:"outputs"`
		} `yaml:"templates"`
	} `yaml:"spec"`
}

func TestBurnContainerSetExposesAllTestsWithoutDuplicatingGPUAllocation(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("09-burn-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)
	renderer, err := template.New("workflow").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
	require.NoError(t, err)

	var rendered bytes.Buffer
	require.NoError(t, renderer.Execute(&rendered, map[string]any{"Namespace": "project-signal"}))

	var workflow workflowManifest
	require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &workflow))

	for _, node := range workflow.Spec.Templates {
		if node.Name != "node-tests" {
			continue
		}

		names := []string{}
		budgets := map[string]resource.Quantity{}

		for _, container := range node.ContainerSet.Containers {
			names = append(names, container.Name)
			require.Len(t, container.Args, 1)
			require.Empty(t, container.Dependencies, "mixed stressors must start concurrently")

			for key, value := range container.Resources.Limits {
				total := budgets[key]
				total.Add(resource.MustParse(value))
				budgets[key] = total
				require.Equal(t, value, container.Resources.Requests[key])
			}
		}

		require.ElementsMatch(t, []string{"main", "cpu-stress", "memory-integrity", "storage-integrity"}, names)

		for key, expected := range map[string]string{"cpu": "8", "memory": "16Gi", "ephemeral-storage": "8Gi"} {
			total := budgets[key]
			require.Zero(t, total.Cmp(resource.MustParse(expected)), key)
		}

		require.Equal(t, 1, strings.Count(node.PodSpecPatch, "nvidia.com/gpu"))
		require.Contains(t, node.PodSpecPatch, `"name":"main"`)

		outputs := map[string]bool{}
		for _, output := range node.Outputs.Parameters {
			outputs[output.Name] = true
		}

		for _, component := range burnComponents {
			for _, suffix := range []string{"verdict", "reason", "measurements"} {
				require.True(t, outputs[component+"-"+suffix], component+"-"+suffix)
			}
		}

		for _, identity := range []string{"test-id", "node-name", "node-uid", "attempt", "script-digest"} {
			require.True(t, outputs[identity], identity)
		}

		return
	}

	t.Fatal("missing node-tests ContainerSet")
}

func TestNativeTestInvocationsRetainIdentityAndEvidence(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		path      string
		templates []string
	}{
		{"04-workflowtemplate.yaml.tmpl", []string{"node-health", "mpi-backend"}},
		{"08-full-node-workflowtemplate.yaml.tmpl", []string{"full-node-stage", "full-node-mpi-backend"}},
	} {
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(test.path)
			require.NoError(t, err)
			renderer, err := template.New("workflow").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
			require.NoError(t, err)

			var rendered bytes.Buffer
			require.NoError(t, renderer.Execute(&rendered, map[string]any{"Namespace": "project-signal"}))

			var workflow workflowManifest
			require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &workflow))

			found := []string{}

			for _, node := range workflow.Spec.Templates {
				if node.Name != test.templates[0] && node.Name != test.templates[1] {
					continue
				}

				found = append(found, node.Name)

				outputs := map[string]bool{}
				for _, output := range node.Outputs.Parameters {
					outputs[output.Name] = true
				}

				for _, identity := range []string{"test-id", "node-name", "node-uid", "attempt", "script-digest", "verdict", "reason", "measurements"} {
					require.True(t, outputs[identity], node.Name+" "+identity)
				}
			}

			require.ElementsMatch(t, test.templates, found)
			require.Contains(t, rendered.String(), "--cascade=foreground")
			require.Contains(t, rendered.String(), "--wait=true")
		})
	}
}

func TestNativeEntrypointsPreserveLegacyScriptsAndRootOutputs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ file, legacyLeaf string }{
		{"04-workflowtemplate.yaml.tmpl", "legacy-node-health"},
		{"08-full-node-workflowtemplate.yaml.tmpl", "legacy-full-node-stage"},
		{"09-burn-workflowtemplate.yaml.tmpl", "legacy-burn-canary"},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(test.file)
			require.NoError(t, err)
			renderer, err := template.New("workflow").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
			require.NoError(t, err)

			var rendered bytes.Buffer
			require.NoError(t, renderer.Execute(&rendered, map[string]any{"Namespace": "project-signal"}))

			var workflow workflowManifest
			require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &workflow))

			defaults := map[string]string{}
			for _, parameter := range workflow.Spec.Arguments.Parameters {
				defaults[parameter.Name] = parameter.Value
			}

			require.Equal(t, "[]", defaults["participant-plan"])
			require.Equal(t, "[]", defaults["test-plan"])

			found := []string{}

			for _, node := range workflow.Spec.Templates {
				switch node.Name {
				case "certification":
					found = append(found, node.Name)
					require.Len(t, node.Steps, 1)
					require.Len(t, node.Steps[0], 2)
					require.Equal(t, "legacy-certification", node.Steps[0][0].Template)
					require.Equal(t, "{{=workflow.parameters['test-plan'] == '[]'}}", node.Steps[0][0].When)
					require.Equal(t, "planned-certification", node.Steps[0][1].Template)
					require.Equal(t, "{{=workflow.parameters['test-plan'] != '[]'}}", node.Steps[0][1].When)
					require.Len(t, node.Outputs.Parameters, 2)

					for _, output := range node.Outputs.Parameters {
						require.Contains(t, []string{"verdict", "reason"}, output.Name)
						require.Contains(t, output.ValueFrom.Expression, "steps.legacy")
						require.Contains(t, output.ValueFrom.Expression, "steps.planned")
						require.Contains(t, output.ValueFrom.Expression, "Succeeded")
					}
				case "planned-certification", "legacy-certification":
					found = append(found, node.Name)
					require.Len(t, node.Outputs.Parameters, 2, node.Name)

					for _, output := range node.Outputs.Parameters {
						require.Contains(t, []string{"verdict", "reason"}, output.Name)
					}
				case test.legacyLeaf:
					found = append(found, node.Name)
					require.Equal(t, []string{"/bin/bash", "/scripts/{{workflow.parameters.script-key}}"}, node.Container.Command)
					require.Empty(t, node.Container.Args, "never pass component arguments to an immutable legacy script")
					require.Empty(t, node.ContainerSet.Containers, "a legacy burn snapshot must execute once")
					require.Len(t, node.Outputs.Parameters, 2)

					for _, output := range node.Outputs.Parameters {
						require.Equal(t, "/results/"+output.Name, output.ValueFrom.Path)
					}
				}
			}

			require.ElementsMatch(t, []string{
				"certification", "planned-certification", "legacy-certification", test.legacyLeaf,
			}, found)
		})
	}
}

func TestRackWorkflowComposesNodeTestsWithoutReplacingParticipantIdentity(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("10-rack-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)
	renderer, err := template.New("workflow").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
	require.NoError(t, err)

	var rendered bytes.Buffer
	require.NoError(t, renderer.Execute(&rendered, map[string]any{"Namespace": "project-signal"}))

	var workflow struct {
		Spec struct {
			Templates []struct {
				Name    string `yaml:"name"`
				Outputs struct {
					Parameters []struct {
						Name      string `yaml:"name"`
						ValueFrom struct {
							Expression string `yaml:"expression"`
						} `yaml:"valueFrom"`
					} `yaml:"parameters"`
				} `yaml:"outputs"`
				DAG struct {
					FailFast bool `yaml:"failFast"`
					Tasks    []struct {
						Name        string `yaml:"name"`
						When        string `yaml:"when"`
						WithParam   string `yaml:"withParam"`
						TemplateRef struct {
							Name     string `yaml:"name"`
							Template string `yaml:"template"`
						} `yaml:"templateRef"`
						Arguments struct {
							Parameters []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"parameters"`
						} `yaml:"arguments"`
					} `yaml:"tasks"`
				} `yaml:"dag"`
			} `yaml:"templates"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &workflow))

	nodeGroups := []string{}

	for _, node := range workflow.Spec.Templates {
		if node.Name != "rack-tests" {
			continue
		}

		require.False(t, node.DAG.FailFast, "retain results for every reserved node")
		require.Len(t, node.Outputs.Parameters, 2)

		for _, output := range node.Outputs.Parameters {
			require.Contains(t, []string{"verdict", "reason"}, output.Name)
			require.Contains(t, output.ValueFrom.Expression, "len(verdicts) == expected")
			require.Contains(t, output.ValueFrom.Expression, "expected > 0")
			require.Contains(t, output.ValueFrom.Expression, "all(verdicts, {# == 'Pass'})")
			require.Contains(t, output.ValueFrom.Expression, "any(verdicts, {# == 'Fail'})")
		}

		for _, task := range node.DAG.Tasks {
			require.True(t, strings.HasPrefix(task.When, "{{="), "evaluate parameter values as strings, not interpolated condition syntax")

			if task.TemplateRef.Name == "" {
				continue
			}

			nodeGroups = append(nodeGroups, task.TemplateRef.Name)
			require.Equal(t, "node-tests", task.TemplateRef.Template)
			require.Equal(t, "{{workflow.parameters.participant-plan}}", task.WithParam)

			parameters := map[string]string{}
			for _, parameter := range task.Arguments.Parameters {
				parameters[parameter.Name] = parameter.Value
			}

			require.Equal(t, "{{item.nodeName}}", parameters["node-name"])
			require.Equal(t, "{{item.nodeUID}}", parameters["node-uid"])

			if task.Name != "identity" {
				require.Equal(t, "{{item.observedGPUCapacity}}", parameters["observed-gpu-capacity"])
				require.Equal(t, "{{item.observedGPUAllocatable}}", parameters["observed-gpu-allocatable"])
			}
		}
	}

	require.ElementsMatch(t, []string{
		"project-signal-gpu-certification",
		"project-signal-gpu-full-node-certification",
		"project-signal-gpu-burn-canary",
	}, nodeGroups)
	require.Contains(t, rendered.String(), "UnsupportedRackTest")
	require.Contains(t, rendered.String(), "exit 1")
}

func TestExplicitRackSampleIsDisabledAndUsesExistingNodeTests(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("samples/rack-explicit-nodes.yaml")
	require.NoError(t, err)

	var profile struct {
		Spec struct {
			Participants struct {
				Count       int      `yaml:"count"`
				NodeNames   []string `yaml:"nodeNames"`
				TopologyKey string   `yaml:"topologyKey"`
			} `yaml:"participants"`
			Workflow struct {
				Name string `yaml:"name"`
			} `yaml:"workflow"`
			Script struct {
				Key string `yaml:"key"`
			} `yaml:"script"`
			RunnerImage string            `yaml:"runnerImage"`
			Parameters  map[string]string `yaml:"parameters"`
			Recurring   struct {
				Enabled   bool     `yaml:"enabled"`
				NodeNames []string `yaml:"nodeNames"`
				ExpiresAt string   `yaml:"expiresAt"`
			} `yaml:"recurring"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &profile))
	require.Equal(t, 2, profile.Spec.Participants.Count)
	require.Len(t, profile.Spec.Participants.NodeNames, profile.Spec.Participants.Count)
	require.Contains(t, profile.Spec.Participants.TopologyKey, "REPLACE_WITH_VERIFIED")

	for _, name := range profile.Spec.Participants.NodeNames {
		require.True(t, strings.HasPrefix(name, "REPLACE_WITH_APPROVED_RACK_NODE_"))
	}

	require.Equal(t, "project-signal-rack-certification", profile.Spec.Workflow.Name)
	require.Equal(t, "project-signal-gpu-full-node-certification", profile.Spec.Parameters["node-workflow-template"])
	require.Equal(t, "gpu-full-node.sh", profile.Spec.Script.Key)
	require.Equal(t, "[]", profile.Spec.Parameters["rack-tests"])
	require.Equal(t, "REPLACE_WITH_VERIFIED_GPU_COUNT", profile.Spec.Parameters["expected-gpu-count"])
	require.True(t, strings.HasPrefix(profile.Spec.RunnerImage, "REPLACE_WITH_"))
	require.False(t, profile.Spec.Recurring.Enabled)
	require.Equal(t, profile.Spec.Participants.NodeNames, profile.Spec.Recurring.NodeNames)
	require.Equal(t, "1970-01-01T00:00:00Z", profile.Spec.Recurring.ExpiresAt)
}

func TestGPUInvocationsRejectFailedQueriesEvenWithPlausibleOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		script string
		reason string
	}{
		{"gpu-identity-example.sh", "GPUIdentityQueryFailed"},
		{"gpu-full-node.sh", "GPUQueryFailed"},
	} {
		t.Run(test.script, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			results := filepath.Join(dir, "results")
			require.NoError(t, os.Mkdir(results, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(
				"#!/usr/bin/env bash\nprintf '%s\\n' 'NVIDIA B200'\nexit 23\n",
			), 0o755))
			script := certificationScript(t, test.script)
			script = strings.ReplaceAll(script, "/results/", results+"/")
			script = strings.ReplaceAll(script, "/dev/termination-log", filepath.Join(dir, "termination-log"))
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)

			cmd.Env = append(
				os.Environ(),
				"PATH="+dir+":"+os.Getenv("PATH"),
				"PROJECT_SIGNAL_STAGE_NAME=workload",
				"EXPECTED_GPU_PRODUCT_REGEX=^NVIDIA B200$",
				"EXPECTED_GPU_COUNT=1",
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			assertFileContents(t, filepath.Join(results, "verdict"), "Fail")
			assertFileContents(t, filepath.Join(results, "reason"), test.reason)
		})
	}
}
