// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	kubeyaml "sigs.k8s.io/yaml"
)

var mpiWorkflowFiles = []string{"04-workflowtemplate.yaml.tmpl", "08-full-node-workflowtemplate.yaml.tmpl"}

func TestMPIPayloadPodsMountSelectedSnapshotAndKeepBackendBoundaries(t *testing.T) {
	t.Parallel()

	for _, file := range mpiWorkflowFiles {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			pods := mpiPodSpecs(t, file)
			require.Len(t, pods, 2)

			for role, pod := range pods {
				require.Len(t, pod.Volumes, 1, role)
				require.Equal(t, "scripts", pod.Volumes[0].Name)
				require.NotNil(t, pod.Volumes[0].ConfigMap)
				require.Equal(t, "{{workflow.parameters.script-configmap}}", pod.Volumes[0].ConfigMap.Name)
				require.EqualValues(t, 0o555, *pod.Volumes[0].ConfigMap.DefaultMode)
				require.Len(t, pod.Containers, 1, role)
				container := pod.Containers[0]
				require.Equal(t, "{{workflow.parameters.runner-image}}", container.Image)
				require.Equal(t, []string{"/bin/bash", "-lc"}, container.Command)
				require.Len(t, container.Args, 1)
				require.Equal(t, []corev1.VolumeMount{{Name: "scripts", MountPath: "/scripts", ReadOnly: true}}, container.VolumeMounts)

				env := map[string]string{}
				for _, variable := range container.Env {
					env[variable.Name] = variable.Value
				}

				require.Equal(t, "{{=workflow.parameters['test-plan'] == '[]' ? 'legacy-inline' : workflow.parameters['mpi-payload-mode']}}",
					env["PROJECT_SIGNAL_MPI_PAYLOAD_MODE"], "an admitted legacy plan must not adopt new script arguments")
				require.Equal(t, "{{workflow.parameters.script-key}}", env["PROJECT_SIGNAL_SCRIPT_KEY"])

				if role == "Launcher" {
					require.Empty(t, container.Resources.Limits)
					require.Equal(t, "{{inputs.parameters.gpu-count}}", env["PROJECT_SIGNAL_MPI_GPU_COUNT"])
				} else {
					gpus := container.Resources.Limits["nvidia.com/gpu"]

					expected := int64(1)
					if file == "08-full-node-workflowtemplate.yaml.tmpl" {
						expected = 8
					}

					require.Equal(t, expected, gpus.Value())
					require.Equal(t, "{{inputs.parameters.node-name}}", pod.NodeSelector["kubernetes.io/hostname"])
				}
			}
		})
	}
}

func TestMPIPayloadDispatchPropagatesFailureWithoutLegacyFallback(t *testing.T) {
	t.Parallel()

	for _, file := range mpiWorkflowFiles {
		for role, pod := range mpiPodSpecs(t, file) {
			for _, outcome := range []string{"pass", "nonzero", "missing", "unsupported"} {
				t.Run(file+"/"+role+"/"+outcome, func(t *testing.T) {
					t.Parallel()

					dir := t.TempDir()

					payload := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\"\nexit \"${SIGNAL_TEST_EXIT:-0}\"\n"
					if outcome != "missing" {
						require.NoError(t, os.WriteFile(filepath.Join(dir, "selected.sh"), []byte(payload), 0o600))
					}

					installLegacyMPIMocks(t, dir)

					mode, exit := "script", "0"
					if outcome == "nonzero" {
						exit = "29"
					}

					if outcome == "unsupported" {
						mode = "unsupported"
					}

					output, err := runMPIWrapper(t, pod.Containers[0], dir, mode, exit)
					require.NotContains(t, string(output), "legacy executable", "external payload failure must not execute an inline substitute")

					switch outcome {
					case "pass":
						require.NoError(t, err, string(output))

						expected := "mpi-worker\n"
						if role == "Launcher" {
							expected = "mpi-launcher\n8\n"
						}

						require.Equal(t, expected, string(output))
					case "nonzero":
						var exitError *exec.ExitError
						require.ErrorAs(t, err, &exitError)
						require.Equal(t, 29, exitError.ExitCode())
					case "missing":
						require.Error(t, err)
						require.Contains(t, string(output), "selected.sh")
					case "unsupported":
						var exitError *exec.ExitError
						require.ErrorAs(t, err, &exitError)
						require.Equal(t, 64, exitError.ExitCode())
						require.Contains(t, string(output), "UnsupportedMPIPayloadMode")
					}
				})
			}
		}
	}
}

func TestLegacyMPIWrapperPreservesLauncherArgvAndWorkerSetup(t *testing.T) {
	t.Parallel()

	for _, file := range mpiWorkflowFiles {
		for role, pod := range mpiPodSpecs(t, file) {
			t.Run(file+"/"+role, func(t *testing.T) {
				t.Parallel()

				dir := t.TempDir()
				installLegacyMPIMocks(t, dir)
				output, err := runMPIWrapper(t, pod.Containers[0], dir, "legacy-inline", "0")
				require.NoError(t, err, string(output))

				expected := []string{
					"legacy executable: mpirun", "--allow-run-as-root", "-np", "1", "--bind-to", "none",
					"--map-by", "slot", "-mca", "plm_rsh_no_tree_spawn", "1", "-mca", "plm_rsh_args",
					"-o ConnectionAttempts=10 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/project-signal-known-hosts",
					"/bin/true",
				}
				if file == "08-full-node-workflowtemplate.yaml.tmpl" {
					expected[3] = "8"
					expected = append(expected[:len(expected)-1], "/bin/bash", "-lc",
						`nvidia-smi -i "${OMPI_COMM_WORLD_LOCAL_RANK}" --query-gpu=index,uuid,name,pci.bus_id,pstate,temperature.gpu --format=csv,noheader,nounits`)
				}

				if role == "Worker" {
					expected = []string{
						"legacy executable: install", "-d", "-m", "0700", "/run/project-signal-ssh",
						"legacy executable: install", "-m", "0600", "/root/.ssh/authorized_keys", "/run/project-signal-ssh/authorized_keys",
						"legacy executable: mkdir", "-p", "/run/sshd",
						"legacy executable: ssh-keygen", "-A",
						"legacy executable: sshd", "-D", "-e", "-o", "AuthorizedKeysFile=/run/project-signal-ssh/authorized_keys",
					}
				}

				require.Equal(t, strings.Join(expected, "\n")+"\n", string(output))

				emptyModeOutput, err := runMPIWrapper(t, pod.Containers[0], dir, "", "0")
				require.NoError(t, err, string(emptyModeOutput))
				require.Equal(t, output, emptyModeOutput, "an empty mode retains the legacy default")

				_, err = runMPIWrapper(t, pod.Containers[0], dir, "legacy-inline", "19")

				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError)
				require.Equal(t, 19, exitError.ExitCode(), "legacy MPI failure must remain an execution failure")
			})
		}
	}
}

func mpiPodSpecs(t *testing.T, file string) map[string]corev1.PodSpec {
	t.Helper()

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	renderer, err := template.New("workflow").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
	require.NoError(t, err)

	var rendered bytes.Buffer
	require.NoError(t, renderer.Execute(&rendered, map[string]string{"Namespace": "project-signal"}))

	var workflow workflowManifest
	require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &workflow))

	defaults := map[string]string{}
	for _, parameter := range workflow.Spec.Arguments.Parameters {
		defaults[parameter.Name] = parameter.Value
	}

	require.Equal(t, "legacy-inline", defaults["mpi-payload-mode"])

	for _, node := range workflow.Spec.Templates {
		if node.Name != "mpi-backend" && node.Name != "full-node-mpi-backend" {
			continue
		}

		require.True(t, node.Resource.SetOwnerReference)
		require.Equal(t, "status.replicaStatuses.Launcher.succeeded == 1", node.Resource.SuccessCondition)
		require.Equal(t, "status.replicaStatuses.Launcher.failed == 1", node.Resource.FailureCondition)

		var job struct {
			Spec struct {
				MPIReplicaSpecs map[string]struct {
					Template corev1.PodTemplateSpec `json:"template"`
				} `json:"mpiReplicaSpecs"`
			} `json:"spec"`
		}

		manifest := strings.NewReplacer(
			"tolerations: {{workflow.parameters.tolerations}}", "tolerations: []",
			"slotsPerWorker: {{inputs.parameters.gpu-count}}", "slotsPerWorker: 8",
			`nvidia.com/gpu: "{{inputs.parameters.gpu-count}}"`, `nvidia.com/gpu: "8"`,
		).Replace(node.Resource.Manifest)
		require.NoError(t, kubeyaml.Unmarshal([]byte(manifest), &job))

		pods := map[string]corev1.PodSpec{}
		for role, replica := range job.Spec.MPIReplicaSpecs {
			pods[role] = replica.Template.Spec
		}

		return pods
	}

	t.Fatal("missing MPI resource template")

	return nil
}

func installLegacyMPIMocks(t *testing.T, dir string) {
	t.Helper()

	for _, executable := range []string{"mpirun", "install", "mkdir", "ssh-keygen", "sshd"} {
		mock := "#!/usr/bin/env bash\nprintf 'legacy executable: %s\\n' \"${0##*/}\"\nprintf '%s\\n' \"$@\"\nexit \"${SIGNAL_TEST_EXIT:-0}\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, executable), []byte(mock), 0o755))
	}
}

func runMPIWrapper(t *testing.T, container corev1.Container, dir, mode, exit string) ([]byte, error) {
	t.Helper()

	wrapper := strings.NewReplacer("/scripts/", dir+"/", "/usr/sbin/sshd", filepath.Join(dir, "sshd")).Replace(container.Args[0])
	cmd := exec.CommandContext(t.Context(), "bash", "-c", wrapper)

	cmd.Env = append(
		os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"PROJECT_SIGNAL_MPI_PAYLOAD_MODE="+mode,
		"PROJECT_SIGNAL_SCRIPT_KEY=selected.sh",
		"PROJECT_SIGNAL_MPI_GPU_COUNT=8",
		"SIGNAL_TEST_EXIT="+exit,
	)

	return cmd.CombinedOutput()
}
