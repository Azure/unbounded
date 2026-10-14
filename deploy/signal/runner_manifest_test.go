// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestRunnerProvidesWorkflowDependencies(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../../images/project-signal-runner/Containerfile")
	require.NoError(t, err)

	containerfile := string(content)
	for _, dependency := range []string{
		"bash",
		"jq",
		"openmpi-bin",
		"openssh-client",
		"openssh-server",
		"python3",
		"PasswordAuthentication no",
		"PermitRootLogin prohibit-password",
		"PubkeyAuthentication yes",
		"UsePAM no",
	} {
		assert.Contains(t, containerfile, dependency)
	}

	assert.Contains(t, containerfile, "nvidia/cuda:")
	assert.Contains(t, containerfile, "@sha256:")
	assert.Contains(t, containerfile, "rm -f /etc/ssh/ssh_host_*")
	assert.NotContains(t, containerfile, "ssh-keygen")
}

func TestWorkflowConfiguresEphemeralMPIHostTrust(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)
	for _, setting := range []string{
		"StrictHostKeyChecking=accept-new",
		"UserKnownHostsFile=/tmp/project-signal-known-hosts",
		"install -d -m 0700 /run/project-signal-ssh",
		"install -m 0600 /root/.ssh/authorized_keys /run/project-signal-ssh/authorized_keys",
		"ssh-keygen -A",
		"AuthorizedKeysFile=/run/project-signal-ssh/authorized_keys",
	} {
		assert.True(t, strings.Contains(manifest, setting), "missing MPI SSH setting %q", setting)
	}

	assert.NotContains(t, manifest, "StrictHostKeyChecking=no")
	assert.NotContains(t, manifest, "StrictModes=no")
}

func TestInlineMPILauncherPreservesHostTrustAndExitStatus(t *testing.T) {
	t.Parallel()

	launcher, _ := inlineMPIPods(t)
	require.Equal(t, []string{"/bin/bash", "-lc"}, launcher.Containers[0].Command)
	require.Len(t, launcher.Containers[0].Args, 1)

	for _, exitCode := range []int{0, 42} {
		t.Run(fmt.Sprintf("exit-%d", exitCode), func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			argumentsFile := filepath.Join(directory, "mpi-arguments")
			require.NoError(t, os.WriteFile(filepath.Join(directory, "mpirun"),
				[]byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SIGNAL_TEST_MPI_ARGS\"\nexit \"$SIGNAL_TEST_MPI_EXIT\"\n"), 0o755))

			command := exec.Command("bash", "-c", launcher.Containers[0].Args[0])

			command.Env = append(
				os.Environ(),
				"PATH="+directory+":"+os.Getenv("PATH"),
				"SIGNAL_TEST_MPI_ARGS="+argumentsFile,
				fmt.Sprintf("SIGNAL_TEST_MPI_EXIT=%d", exitCode),
			)
			output, err := command.CombinedOutput()

			if exitCode == 0 {
				require.NoError(t, err, "%s", output)
			} else {
				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError)
				assert.Equal(t, exitCode, exitError.ExitCode())
			}

			content, err := os.ReadFile(argumentsFile)
			require.NoError(t, err)
			assert.Equal(t, []string{
				"--allow-run-as-root", "-np", "1", "--bind-to", "none",
				"--map-by", "slot", "-mca", "plm_rsh_no_tree_spawn", "1",
				"-mca", "plm_rsh_args",
				"-o ConnectionAttempts=10 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/project-signal-known-hosts",
				"/bin/true",
			}, strings.Split(strings.TrimSuffix(string(content), "\n"), "\n"))
		})
	}
}

func TestInlineMPIWorkerCopiesProjectedKeysBeforeStartingSSH(t *testing.T) {
	t.Parallel()

	_, worker := inlineMPIPods(t)
	require.Len(t, worker.Containers[0].Command, 3)
	assert.Equal(t, []string{"/bin/bash", "-lc"}, worker.Containers[0].Command[:2])

	for _, tc := range []struct {
		name       string
		mode       os.FileMode
		missing    bool
		symlink    bool
		keygenExit int
	}{
		{name: "projected public file", mode: 0o644},
		{name: "read-only projection", mode: 0o444},
		{name: "symlinked projection", mode: 0o644, symlink: true},
		{name: "missing projection", missing: true},
		{name: "host key generation failure", mode: 0o644, keygenExit: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			authorizedKeys := filepath.Join(directory, "operator-authorized-keys")
			runtimeKeys := filepath.Join(directory, "runtime-ssh")
			hostKeys := filepath.Join(directory, "generated-host-keys")
			daemonArguments := filepath.Join(directory, "sshd-arguments")
			daemon := filepath.Join(directory, "sshd")

			if !tc.missing {
				projected := authorizedKeys
				if tc.symlink {
					projected = filepath.Join(directory, "projected-public-key")
					require.NoError(t, os.Symlink(projected, authorizedKeys))
				}

				require.NoError(t, os.WriteFile(projected, []byte("example operator-projected public key\n"), tc.mode))
			}

			require.NoError(t, os.WriteFile(filepath.Join(directory, "ssh-keygen"),
				[]byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SIGNAL_TEST_HOST_KEYS\"\nexit \"$SIGNAL_TEST_KEYGEN_EXIT\"\n"), 0o755))
			require.NoError(t, os.WriteFile(daemon,
				[]byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SIGNAL_TEST_SSHD_ARGS\"\n"), 0o755))

			body := strings.NewReplacer(
				"/run/project-signal-ssh", shellQuote(runtimeKeys),
				"/root/.ssh/authorized_keys", shellQuote(authorizedKeys),
				"/run/sshd", shellQuote(filepath.Join(directory, "runtime-sshd")),
				"/usr/sbin/sshd", shellQuote(daemon),
			).Replace(worker.Containers[0].Command[2])
			command := exec.Command("bash", "-c", body)

			command.Env = append(
				os.Environ(),
				"PATH="+directory+":"+os.Getenv("PATH"),
				"SIGNAL_TEST_HOST_KEYS="+hostKeys,
				"SIGNAL_TEST_SSHD_ARGS="+daemonArguments,
				fmt.Sprintf("SIGNAL_TEST_KEYGEN_EXIT=%d", tc.keygenExit),
			)
			output, err := command.CombinedOutput()

			if tc.missing {
				require.Error(t, err)
				assert.NoFileExists(t, hostKeys)
				assert.NoFileExists(t, daemonArguments)

				return
			}

			if tc.keygenExit != 0 {
				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError)
				assert.Equal(t, tc.keygenExit, exitError.ExitCode())
				assert.NoFileExists(t, daemonArguments)
			} else {
				require.NoError(t, err, "%s", output)
				assertFileContents(t, daemonArguments, "-D\n-e\n-o\nAuthorizedKeysFile="+runtimeKeys+"/authorized_keys\n")
			}

			assertFileContents(t, filepath.Join(runtimeKeys, "authorized_keys"), "example operator-projected public key\n")
			copied, err := os.Lstat(filepath.Join(runtimeKeys, "authorized_keys"))
			require.NoError(t, err)
			assert.True(t, copied.Mode().IsRegular())
			assert.Equal(t, os.FileMode(0o600), copied.Mode().Perm())

			privateDirectory, err := os.Stat(runtimeKeys)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), privateDirectory.Mode().Perm())
			assertFileContents(t, hostKeys, "-A\n")
		})
	}
}

func inlineMPIPods(t *testing.T) (corev1.PodSpec, corev1.PodSpec) {
	t.Helper()

	output := renderSignalManifests(t, nil)

	var workflow struct {
		Spec struct {
			ServiceAccountName string `json:"serviceAccountName"`
			Templates          []struct {
				Name     string `json:"name"`
				Resource *struct {
					Action            string `json:"action"`
					SetOwnerReference bool   `json:"setOwnerReference"`
					SuccessCondition  string `json:"successCondition"`
					FailureCondition  string `json:"failureCondition"`
					Manifest          string `json:"manifest"`
				} `json:"resource"`
			} `json:"templates"`
		} `json:"spec"`
	}
	readSignalManifest(t, output, "04-workflowtemplate.yaml", &workflow)
	assert.Equal(t, "project-signal-runner", workflow.Spec.ServiceAccountName)

	for _, template := range workflow.Spec.Templates {
		if template.Name != "mpi-backend" {
			continue
		}

		require.NotNil(t, template.Resource)
		assert.Equal(t, "create", template.Resource.Action)
		assert.True(t, template.Resource.SetOwnerReference)
		assert.Equal(t, "status.replicaStatuses.Launcher.succeeded == 1", template.Resource.SuccessCondition)
		assert.Equal(t, "status.replicaStatuses.Launcher.failed == 1", template.Resource.FailureCondition)
		manifest := strings.NewReplacer(
			"{{workflow.parameters.run-name}}", "identity-test",
			"{{workflow.parameters.runner-image}}", "example.invalid/runner:reviewed",
			"{{workflow.parameters.participants}}", "approved-node",
			"{{workflow.parameters.tolerations}}", `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`,
		).Replace(template.Resource.Manifest)

		var job struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Spec       struct {
				SlotsPerWorker  int `json:"slotsPerWorker"`
				MPIReplicaSpecs map[string]struct {
					Replicas int                    `json:"replicas"`
					Template corev1.PodTemplateSpec `json:"template"`
				} `json:"mpiReplicaSpecs"`
			} `json:"spec"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(manifest), &job))
		assert.Equal(t, "kubeflow.org/v2beta1", job.APIVersion)
		assert.Equal(t, "MPIJob", job.Kind)
		assert.Equal(t, 1, job.Spec.SlotsPerWorker)
		assert.Equal(t, 1, job.Spec.MPIReplicaSpecs["Launcher"].Replicas)
		assert.Equal(t, 1, job.Spec.MPIReplicaSpecs["Worker"].Replicas)
		launcher := job.Spec.MPIReplicaSpecs["Launcher"].Template.Spec
		worker := job.Spec.MPIReplicaSpecs["Worker"].Template.Spec

		require.Len(t, launcher.Containers, 1)
		require.Len(t, worker.Containers, 1)
		assert.Equal(t, "approved-node", worker.NodeSelector["kubernetes.io/hostname"])
		require.Len(t, worker.Containers[0].Resources.Limits, 1)
		gpu := worker.Containers[0].Resources.Limits["nvidia.com/gpu"]
		assert.Equal(t, int64(1), gpu.Value())
		assert.Equal(t, []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			worker.Tolerations)
		assert.False(t, worker.HostPID)
		assert.False(t, worker.HostNetwork)

		return launcher, worker
	}

	t.Fatal("inline MPI backend template not found")

	return corev1.PodSpec{}, corev1.PodSpec{}
}
