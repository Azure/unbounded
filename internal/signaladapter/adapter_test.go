// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestRenderOrdinaryPodsPerParticipant(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = ""

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	if len(resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(resources))
	}

	for index, resource := range resources {
		if resource.GetKind() != "Pod" {
			t.Fatalf("resource %d kind = %q", index, resource.GetKind())
		}

		nodeName, _, err := unstructured.NestedString(resource.Object, "spec", "nodeSelector", corev1.LabelHostname)
		if err != nil {
			t.Fatal(err)
		}

		if nodeName != participants[index].NodeName {
			t.Fatalf("resource %d node selector = %q", index, nodeName)
		}

		restartPolicy, _, err := unstructured.NestedString(resource.Object, "spec", "restartPolicy")
		if err != nil {
			t.Fatal(err)
		}

		if restartPolicy != string(corev1.RestartPolicyNever) {
			t.Fatalf("resource %d restart policy = %q", index, restartPolicy)
		}

		automount, _, err := unstructured.NestedBool(resource.Object, "spec", "automountServiceAccountToken")
		if err != nil {
			t.Fatal(err)
		}

		if automount {
			t.Fatalf("resource %d automounts a service account token", index)
		}

		hostNetwork, _, err := unstructured.NestedBool(resource.Object, "spec", "hostNetwork")
		if err != nil {
			t.Fatal(err)
		}

		if hostNetwork {
			t.Fatalf("resource %d unexpectedly uses the host network", index)
		}

		if resource.GetAnnotations()[requiredTopologyKey] != "" {
			t.Fatalf("resource %d unexpectedly requests Kueue topology", index)
		}

		activeDeadline, _, err := unstructured.NestedInt64(resource.Object, "spec", "activeDeadlineSeconds")
		if err != nil || activeDeadline != config.BackendDeadlineSeconds {
			t.Fatalf("resource %d active deadline = %d, err = %v", index, activeDeadline, err)
		}
	}
}

func TestRenderOrdinaryPodWithKueueQueue(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = "certification"

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("runtime stage is participant only", func(t *testing.T) {
		t.Parallel()

		config := testConfig()
		config.ScriptRequiresStage = true
		config.RuntimeStageName = "collective-bandwidth"

		participants, tolerations, environment, err := ValidateConfig(config)
		if err != nil {
			t.Fatal(err)
		}

		config.BackendAPIVersion = "v1"
		config.BackendKind = "Pod"
		config.Queue = ""

		pods, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
		if err != nil {
			t.Fatal(err)
		}

		podJSON, err := json.Marshal(pods[0].Object)
		if err != nil {
			t.Fatal(err)
		}

		var pod corev1.Pod
		if err := json.Unmarshal(podJSON, &pod); err != nil {
			t.Fatal(err)
		}

		if value := environmentValue(pod.Spec.Containers[0].Env, "PROJECT_SIGNAL_STAGE_NAME"); value != config.RuntimeStageName {
			t.Fatalf("Pod runtime stage = %q", value)
		}

		config.BackendAPIVersion = "kubeflow.org/v2beta1"
		config.BackendKind = "MPIJob"
		config.Queue = "certification"

		jobs, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
		if err != nil {
			t.Fatal(err)
		}

		launcher, _, err := unstructured.NestedSlice(
			jobs[0].Object,
			"spec",
			"mpiReplicaSpecs",
			"Launcher",
			"template",
			"spec",
			"containers",
		)
		if err != nil {
			t.Fatal(err)
		}

		worker, _, err := unstructured.NestedSlice(
			jobs[0].Object,
			"spec",
			"mpiReplicaSpecs",
			"Worker",
			"template",
			"spec",
			"containers",
		)
		if err != nil {
			t.Fatal(err)
		}

		launcherJSON, _ := json.Marshal(launcher)
		workerJSON, _ := json.Marshal(worker)

		if strings.Contains(string(launcherJSON), "PROJECT_SIGNAL_STAGE_NAME") {
			t.Fatalf("MPI launcher received runtime stage: %s", launcherJSON)
		}

		if !strings.Contains(string(workerJSON), `"name":"PROJECT_SIGNAL_STAGE_NAME","value":"collective-bandwidth"`) {
			t.Fatalf("MPI worker runtime stage missing: %s", workerJSON)
		}
	})

	if queue := resources[0].GetLabels()["kueue.x-k8s.io/queue-name"]; queue != "certification" {
		t.Fatalf("Pod queue label = %q", queue)
	}
}

func TestRenderMPIJobsPerParticipant(t *testing.T) {
	t.Parallel()

	config := testConfig()

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	for index, resource := range resources {
		if resource.GetAPIVersion() != "kubeflow.org/v2beta1" || resource.GetKind() != "MPIJob" {
			t.Fatalf("resource %d = %s %s", index, resource.GetAPIVersion(), resource.GetKind())
		}

		queue, _, err := unstructured.NestedString(
			resource.Object,
			"metadata",
			"labels",
			"kueue.x-k8s.io/queue-name",
		)
		if err != nil {
			t.Fatal(err)
		}

		if queue != "certification" {
			t.Fatalf("resource %d queue = %q", index, queue)
		}

		activeDeadline, _, err := unstructured.NestedInt64(resource.Object, "spec", "runPolicy", "activeDeadlineSeconds")
		if err != nil {
			t.Fatal(err)
		}

		if activeDeadline != config.BackendDeadlineSeconds {
			t.Fatalf("resource %d active deadline = %d", index, activeDeadline)
		}

		cleanPolicy, _, err := unstructured.NestedString(resource.Object, "spec", "runPolicy", "cleanPodPolicy")
		if err != nil {
			t.Fatal(err)
		}

		if cleanPolicy != "All" {
			t.Fatalf("resource %d cleanPodPolicy = %q", index, cleanPolicy)
		}

		for _, replica := range []string{"Launcher", "Worker"} {
			replicaRestartPolicy, _, err := unstructured.NestedString(
				resource.Object,
				"spec",
				"mpiReplicaSpecs",
				replica,
				"restartPolicy",
			)
			if err != nil {
				t.Fatal(err)
			}

			if replicaRestartPolicy != string(corev1.RestartPolicyNever) {
				t.Fatalf("resource %d %s replica restart policy = %q", index, replica, replicaRestartPolicy)
			}

			restartPolicy, _, err := unstructured.NestedString(
				resource.Object,
				"spec",
				"mpiReplicaSpecs",
				replica,
				"template",
				"spec",
				"restartPolicy",
			)
			if err != nil {
				t.Fatal(err)
			}

			if restartPolicy != string(corev1.RestartPolicyNever) {
				t.Fatalf("resource %d %s restart policy = %q", index, replica, restartPolicy)
			}
		}

		if _, found, err := unstructured.NestedString(resource.Object, "spec", "restartPolicy"); err != nil || found {
			t.Fatalf("resource %d has top-level restartPolicy: found=%t err=%v", index, found, err)
		}

		workerNode, _, err := unstructured.NestedString(
			resource.Object,
			"spec",
			"mpiReplicaSpecs",
			"Worker",
			"template",
			"spec",
			"nodeSelector",
			corev1.LabelHostname,
		)
		if err != nil {
			t.Fatal(err)
		}

		if workerNode != participants[index].NodeName {
			t.Fatalf("resource %d worker node = %q", index, workerNode)
		}

		launcherContainers, found, err := unstructured.NestedSlice(
			resource.Object,
			"spec",
			"mpiReplicaSpecs",
			"Launcher",
			"template",
			"spec",
			"containers",
		)
		if err != nil || !found {
			t.Fatalf("resource %d launcher containers: found=%t err=%v", index, found, err)
		}

		launcherJSON, err := json.Marshal(launcherContainers)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(launcherJSON), "nvidia.com/gpu") {
			t.Fatalf("resource %d launcher requests GPUs: %s", index, launcherJSON)
		}

		if strings.Contains(string(launcherJSON), "/etc/mpi/hostfile") ||
			!strings.Contains(string(launcherJSON), mpiHostfilePath) ||
			!strings.Contains(string(launcherJSON), "OMPI_MCA_orte_default_hostfile") ||
			!strings.Contains(string(launcherJSON), "BatchMode=yes") ||
			!strings.Contains(string(launcherJSON), "ConnectionAttempts=1") ||
			!strings.Contains(string(launcherJSON), "StrictHostKeyChecking=accept-new") ||
			!strings.Contains(string(launcherJSON), "UserKnownHostsFile=") ||
			!strings.Contains(string(launcherJSON), mpiKnownHostsPath) ||
			!strings.Contains(string(launcherJSON), "/bin/true") ||
			!strings.Contains(string(launcherJSON), "Project Signal MPI readiness timed out") {
			t.Fatalf("resource %d launcher readiness fence missing: %s", index, launcherJSON)
		}

		jobName := resource.GetName()

		expectedHost := fmt.Sprintf(
			"%s-worker-0.%s.%s.svc",
			jobName,
			jobName,
			config.Namespace,
		)
		if !strings.Contains(string(launcherJSON), expectedHost+" slots="+config.GPUResourceCount) {
			t.Fatalf("resource %d launcher hostfile entry missing: %s", index, launcherJSON)
		}
	}
}

func TestMPIWorkerReadinessTimeoutIsBoundedByBackendDeadline(t *testing.T) {
	t.Parallel()

	for _, backendDeadline := range []int64{2, 60, 121, 540} {
		timeout := mpiWorkerReadinessTimeout(backendDeadline)
		if timeout < 1 || timeout >= backendDeadline || timeout > maxMPIWorkerReadyWait {
			t.Fatalf("mpiWorkerReadinessTimeout(%d) = %d", backendDeadline, timeout)
		}
	}
}

func TestMPIWorkerReadinessFenceWaitsForWorkers(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	hostfile := filepath.Join(directory, "hostfile")
	readyFile := filepath.Join(directory, "ready")
	argumentsFile := filepath.Join(directory, "ssh-arguments")
	binDirectory := fakeSSH(t, directory)
	ready := make(chan error, 1)
	hosts := []mpiWorkerHost{
		{Name: "worker-0.job.namespace.svc", Slots: 2},
		{Name: "worker-1.job.namespace.svc", Slots: 2},
	}

	go func() {
		time.Sleep(100 * time.Millisecond)

		ready <- os.WriteFile(readyFile, nil, 0o600)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, "/bin/bash", "-c", mpiLauncherPrelude(hostfile, hosts, 5))

	command.Env = append(
		os.Environ(),
		"PATH="+binDirectory+":"+os.Getenv("PATH"),
		"PROJECT_SIGNAL_TEST_SSH_READY="+readyFile,
		"PROJECT_SIGNAL_TEST_SSH_ARGUMENTS="+argumentsFile,
	)

	var stderr bytes.Buffer

	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		t.Fatalf("readiness fence failed: %v stderr=%q", err, stderr.String())
	}

	if err := <-ready; err != nil {
		t.Fatal(err)
	}

	hostfileContent, err := os.ReadFile(hostfile)
	if err != nil {
		t.Fatal(err)
	}

	if string(hostfileContent) != "worker-0.job.namespace.svc slots=2\nworker-1.job.namespace.svc slots=2\n" {
		t.Fatalf("generated hostfile = %q", hostfileContent)
	}

	arguments, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, expected := range []string{
		"-o BatchMode=yes",
		"-o ConnectTimeout=2",
		"-o ConnectionAttempts=1",
		"-o StrictHostKeyChecking=accept-new",
		"-o UserKnownHostsFile=" + mpiKnownHostsPath,
		"worker-1.job.namespace.svc /bin/true",
	} {
		if !strings.Contains(string(arguments), expected) {
			t.Fatalf("SSH arguments %q do not contain %q", arguments, expected)
		}
	}
}

func TestMPIWorkerReadinessFenceTimesOut(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	hostfile := filepath.Join(directory, "hostfile")
	command := exec.Command(
		"/bin/bash",
		"-c",
		mpiLauncherPrelude(
			hostfile,
			[]mpiWorkerHost{{Name: "worker-0.job.namespace.svc", Slots: 1}},
			2,
		),
	)

	command.Env = append(os.Environ(), "PATH="+fakeSSH(t, directory)+":"+os.Getenv("PATH"))

	var stderr bytes.Buffer

	command.Stderr = &stderr

	err := command.Run()
	if err == nil {
		t.Fatal("readiness fence succeeded without a listening worker")
	}

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 124 {
		t.Fatalf("readiness fence error = %v, want exit 124", err)
	}

	if !strings.Contains(stderr.String(), "timed out after 2s") ||
		!strings.Contains(stderr.String(), hostfile) ||
		!strings.Contains(stderr.String(), "last unready host=worker-0.job.namespace.svc") ||
		!strings.Contains(stderr.String(), "simulated SSH connection failure") {
		t.Fatalf("readiness timeout stderr = %q", stderr.String())
	}
}

func TestMPILauncherPreludeIgnoresOperatorHostfile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	operatorHostfile := filepath.Join(directory, "operator-hostfile")
	if err := os.WriteFile(operatorHostfile, []byte("stale-worker slots=99\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	generatedHostfile := filepath.Join(directory, "generated-hostfile")
	readyFile := filepath.Join(directory, "ready")
	observedOpenMPIHostfile := filepath.Join(directory, "observed-openmpi-hostfile")

	if err := os.WriteFile(readyFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(
		"/bin/bash",
		"-c",
		mpiLauncherPrelude(
			generatedHostfile,
			[]mpiWorkerHost{{Name: "worker-0.job.namespace.svc", Slots: 4}},
			2,
		),
	)

	command.Env = append(
		os.Environ(),
		"PATH="+fakeSSH(t, directory)+":"+os.Getenv("PATH"),
		"PROJECT_SIGNAL_TEST_SSH_READY="+readyFile,
		"PROJECT_SIGNAL_TEST_OMPI_HOSTFILE_OUTPUT="+observedOpenMPIHostfile,
		"OMPI_MCA_orte_default_hostfile="+operatorHostfile,
	)

	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("launcher prelude failed: %v output=%q", err, output)
	}

	content, err := os.ReadFile(generatedHostfile)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(content), "stale-worker") ||
		string(content) != "worker-0.job.namespace.svc slots=4\n" {
		t.Fatalf("generated hostfile = %q", content)
	}

	observed, err := os.ReadFile(observedOpenMPIHostfile)
	if err != nil {
		t.Fatal(err)
	}

	if string(observed) != generatedHostfile {
		t.Fatalf("OpenMPI hostfile = %q, want %q", observed, generatedHostfile)
	}
}

func TestMPIWorkerHostsRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		namespace   string
		jobName     string
		workerCount int64
		slots       int64
	}{
		"zero workers":   {namespace: "namespace", jobName: "job", slots: 1},
		"zero slots":     {namespace: "namespace", jobName: "job", workerCount: 1},
		"invalid host":   {namespace: "namespace", jobName: "INVALID", workerCount: 1, slots: 1},
		"too many hosts": {namespace: "namespace", jobName: "job", workerCount: MaxParticipants + 1, slots: 1},
		"too many slots": {namespace: "namespace", jobName: "job", workerCount: 1, slots: maxMPIWorkerSlots + 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := mpiWorkerHosts(
				test.namespace,
				test.jobName,
				test.workerCount,
				test.slots,
			); err == nil {
				t.Fatal("mpiWorkerHosts accepted invalid input")
			}
		})
	}
}

func fakeSSH(t *testing.T, directory string) string {
	t.Helper()

	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0o750); err != nil {
		t.Fatal(err)
	}

	content := []byte(`#!/bin/bash
	if [[ -n "${PROJECT_SIGNAL_TEST_SSH_ARGUMENTS:-}" ]]; then
	  printf '%s\n' "$*" >> "${PROJECT_SIGNAL_TEST_SSH_ARGUMENTS}"
fi
	if [[ -n "${PROJECT_SIGNAL_TEST_OMPI_HOSTFILE_OUTPUT:-}" ]]; then
	  printf '%s' "${OMPI_MCA_orte_default_hostfile:-}" > "${PROJECT_SIGNAL_TEST_OMPI_HOSTFILE_OUTPUT}"
	fi
	required=(
	  "-o BatchMode=yes"
	  "-o ConnectTimeout=2"
	  "-o ConnectionAttempts=1"
	  "-o StrictHostKeyChecking=accept-new"
	  "-o UserKnownHostsFile=/tmp/project-signal-known-hosts"
	)
	arguments=" $* "
	for option in "${required[@]}"; do
	  if [[ "${arguments}" != *" ${option} "* ]]; then
	    printf 'missing SSH option: %s\n' "${option}" >&2
	    exit 255
	  fi
	done
	if [[ -n "${PROJECT_SIGNAL_TEST_SSH_READY:-}" && -f "${PROJECT_SIGNAL_TEST_SSH_READY}" ]]; then
	  exit 0
	fi
	printf 'simulated SSH connection failure for %s\n' "$*" >&2
	exit 255
	`)
	if err := os.WriteFile(filepath.Join(binDirectory, "ssh"), content, 0o750); err != nil {
		t.Fatal(err)
	}

	return binDirectory
}

func TestRenderMPIJobUsesExplicitLifecycleOverrides(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.ScriptEnvironmentJSON = `{
			"BACKEND_BACKOFF_LIMIT":"2",
			"BACKEND_TTL_SECONDS_AFTER_FINISHED":"7200"
		}`

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	backoff, _, err := unstructured.NestedInt64(resources[0].Object, "spec", "runPolicy", "backoffLimit")
	if err != nil {
		t.Fatal(err)
	}

	ttl, _, err := unstructured.NestedInt64(resources[0].Object, "spec", "runPolicy", "ttlSecondsAfterFinished")
	if err != nil {
		t.Fatal(err)
	}

	if backoff != 2 || ttl != 7200 {
		t.Fatalf("run policy backoff=%d ttl=%d", backoff, ttl)
	}
}

func TestRenderWorkloadPolicyOnParticipantContainers(t *testing.T) {
	t.Parallel()

	policy := signalv1.WorkloadPolicy{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(0)),
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"IPC_LOCK"},
			},
		},
		HostNetwork: true,
		KueueTopology: &signalv1.KueueTopologyPolicy{
			RequiredLevel: corev1.LabelHostname,
		},
		WorkingDir: "/opt/diagnostic",
		Results:    &corev1.EmptyDirVolumeSource{SizeLimit: quantity("2Gi")},
		Volumes: []signalv1.WorkloadVolume{
			{
				Name:      "host-proc",
				MountPath: "/host/proc",
				ReadOnly:  true,
				HostPath:  &corev1.HostPathVolumeSource{Path: "/proc"},
			},
			{
				Name:      "work",
				MountPath: "/work",
				EmptyDir:  &corev1.EmptyDirVolumeSource{SizeLimit: quantity("8Gi")},
			},
		},
	}

	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = "certification"
	config.GPUResourceCount = "8"
	config.WorkloadPolicyJSON = string(policyJSON)

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resources[0].Object, &pod); err != nil {
		t.Fatal(err)
	}

	container := pod.Spec.Containers[0]

	gpuLimit := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if !pod.Spec.HostNetwork ||
		pod.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet ||
		pod.Annotations[requiredTopologyKey] != corev1.LabelHostname ||
		container.WorkingDir != "/opt/diagnostic" ||
		container.Resources.Requests.Cpu().String() != "8" ||
		container.Resources.Limits.Memory().String() != "32Gi" ||
		gpuLimit.String() != "8" ||
		container.SecurityContext == nil ||
		container.SecurityContext.RunAsUser == nil ||
		*container.SecurityContext.RunAsUser != 0 ||
		len(container.SecurityContext.Capabilities.Add) != 1 {
		t.Fatalf("participant container = %#v", container)
	}

	if len(pod.Spec.Volumes) != 4 ||
		pod.Spec.Volumes[1].EmptyDir.SizeLimit.String() != "2Gi" ||
		pod.Spec.Volumes[2].HostPath.Path != "/proc" ||
		pod.Spec.Volumes[3].EmptyDir.SizeLimit.String() != "8Gi" {
		t.Fatalf("participant volumes = %#v", pod.Spec.Volumes)
	}
}

func TestMPIWorkloadPolicyDoesNotLeakOntoLauncher(t *testing.T) {
	t.Parallel()

	policyJSON := `{
			"hostNetwork":true,
			"kueueTopology":{"requiredLevel":"kubernetes.io/hostname"},
			"resources":{"requests":{"cpu":"4"},"limits":{"cpu":"4"}},
			"volumes":[{"name":"host-device","mountPath":"/dev/infiniband","hostPath":{"path":"/dev/infiniband"}}]
		}`
	config := testConfig()
	config.WorkloadPolicyJSON = policyJSON

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner())
	if err != nil {
		t.Fatal(err)
	}

	content, err := json.Marshal(resources[0].Object)
	if err != nil {
		t.Fatal(err)
	}

	var job struct {
		Spec struct {
			MPIReplicaSpecs map[string]struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"mpiReplicaSpecs"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(content, &job); err != nil {
		t.Fatal(err)
	}

	launcherTemplate := job.Spec.MPIReplicaSpecs["Launcher"].Template
	workerTemplate := job.Spec.MPIReplicaSpecs["Worker"].Template
	launcher := launcherTemplate.Spec
	worker := workerTemplate.Spec

	if launcher.HostNetwork ||
		launcher.DNSPolicy == corev1.DNSClusterFirstWithHostNet ||
		launcherTemplate.Annotations[requiredTopologyKey] != "" ||
		len(launcher.Volumes) != 2 ||
		len(launcher.Containers[0].VolumeMounts) != 2 {
		t.Fatalf("launcher received participant policy: %#v", launcher)
	}

	if !worker.HostNetwork ||
		worker.DNSPolicy != corev1.DNSClusterFirstWithHostNet ||
		workerTemplate.Annotations[requiredTopologyKey] != corev1.LabelHostname ||
		len(worker.Volumes) != 3 ||
		worker.Volumes[2].HostPath.Path != "/dev/infiniband" ||
		worker.Containers[0].Resources.Requests.Cpu().String() != "4" {
		t.Fatalf("worker policy = %#v", worker)
	}
}

func TestValidateConfigRejectsKueueTopologyWithoutQueue(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = ""
	config.WorkloadPolicyJSON = `{"kueueTopology":{"requiredLevel":"kubernetes.io/hostname"}}`

	if _, _, _, err := ValidateConfig(config); err == nil ||
		!strings.Contains(err.Error(), "kueueTopology requires a queue") {
		t.Fatalf("ValidateConfig topology error = %v", err)
	}
}

func TestValidateConfigRejectsMPIWorkerReadinessWithoutTimeBudget(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendDeadlineSeconds = 1

	if _, _, _, err := ValidateConfig(config); err == nil ||
		!strings.Contains(err.Error(), "at least 2 for MPIJob worker readiness") {
		t.Fatalf("ValidateConfig deadline error = %v", err)
	}
}

func TestRenderRejectsGPUResourceInWorkloadPolicy(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.WorkloadPolicyJSON = `{"resources":{"limits":{"nvidia.com/gpu":"2"}}}`

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderResources(
		config,
		participants,
		tolerations,
		environment,
		testWorkflowOwner(),
	); err == nil || !strings.Contains(err.Error(), "gpu-resource-count") {
		t.Fatalf("RenderResources error = %v", err)
	}
}

func TestParseParticipantsRejectsMalformedIdentity(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"noncontiguous index": `[{"index":1,"nodeName":"node-a","nodeUID":"uid-a"}]`,
		"missing UID":         `[{"index":0,"nodeName":"node-a","nodeUID":""}]`,
		"duplicate node":      `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"},{"index":1,"nodeName":"node-a","nodeUID":"uid-b"}]`,
		"duplicate UID":       `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"},{"index":1,"nodeName":"node-b","nodeUID":"uid-a"}]`,
	}

	for name, participantsJSON := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			count := 1
			if strings.Contains(participantsJSON, `"index":1`) && strings.Count(participantsJSON, `"index"`) == 2 {
				count = 2
			}

			if _, err := ParseParticipants(participantsJSON, count, "", ""); err == nil {
				t.Fatal("ParseParticipants succeeded")
			}
		})
	}
}

func TestParseParticipantsRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	participantsJSON := `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"}] []`
	if _, err := ParseParticipants(participantsJSON, 1, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted trailing JSON")
	}
}

func TestParseParticipantsCompatibilityFallbacks(t *testing.T) {
	t.Parallel()

	for name, compatibility := range map[string][2]string{
		"request Node": {"node-request", ""},
		"legacy list":  {"", "node-legacy,node-ignored"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			participants, err := ParseParticipants("", 1, compatibility[0], compatibility[1])
			if err != nil {
				t.Fatal(err)
			}

			if len(participants) != 1 || participants[0].NodeName == "" {
				t.Fatalf("participants = %#v", participants)
			}
		})
	}
}

func TestValidateConfigRequiresDigestAndMPIQueue(t *testing.T) {
	t.Parallel()

	config := testConfig()

	config.RunnerImage = "example.invalid/runner:latest"
	if _, _, _, err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "immutable sha256") {
		t.Fatalf("digest validation error = %v", err)
	}

	config = testConfig()

	config.Queue = ""
	if _, _, _, err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "queue is required") {
		t.Fatalf("queue validation error = %v", err)
	}
}

func TestValidateConfigRequiresDeclaredRuntimeStage(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.ScriptRequiresStage = true

	if _, _, _, err := ValidateConfig(config); err == nil ||
		!strings.Contains(err.Error(), "runtime stage is required") {
		t.Fatalf("ValidateConfig runtime stage error = %v", err)
	}

	config.RuntimeStageName = "Collective/Bandwidth"
	if _, _, _, err := ValidateConfig(config); err == nil ||
		!strings.Contains(err.Error(), "runtime stage name is invalid") {
		t.Fatalf("ValidateConfig malformed stage error = %v", err)
	}
}

func TestValidateConfigAcceptsControllerScriptDigest(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.ScriptDigest = "sha256:3c11c8ff7b1182a1215a3ee4a0512901116c45646669b08a46710795120601a8"

	if _, _, _, err := ValidateConfig(config); err != nil {
		t.Fatalf("ValidateConfig rejected controller script digest: %v", err)
	}
}

func TestValidateConfigRejectsMalformedScriptDigest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("b", 64)
	for name, digest := range map[string]string{
		"bare hex":         strings.Repeat("b", 64),
		"uppercase hex":    "sha256:" + strings.Repeat("B", 64),
		"wrong algorithm":  "sha512:" + strings.Repeat("b", 64),
		"short":            "sha256:" + strings.Repeat("b", 63),
		"long":             "sha256:" + strings.Repeat("b", 65),
		"leading space":    " " + valid,
		"trailing newline": valid + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			config := testConfig()
			config.ScriptDigest = digest

			if _, _, _, err := ValidateConfig(config); err == nil ||
				!strings.Contains(err.Error(), "algorithm-qualified SHA-256 digest") {
				t.Fatalf("ValidateConfig script digest error = %v", err)
			}
		})
	}
}

func TestValidateConfigRejectsReservedEnvironment(t *testing.T) {
	t.Parallel()

	for _, environment := range []string{
		`{"PATH":"/tmp"}`,
		`{"PROJECT_SIGNAL_RUN_UID":"replacement"}`,
	} {
		config := testConfig()
		config.ScriptEnvironmentJSON = environment

		if _, _, _, err := ValidateConfig(config); err == nil ||
			!strings.Contains(err.Error(), "reserved name") {
			t.Fatalf("environment %s error = %v", environment, err)
		}
	}
}

func TestAggregatePassRequiresCompleteResultsAndIdentities(t *testing.T) {
	t.Parallel()

	resources := testBackendResources()
	results := []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	}

	output, err := Aggregate(2, resources, results)
	if err != nil {
		t.Fatal(err)
	}

	if output.Verdict != "Pass" || output.Reason != "AllParticipantsPassed" {
		t.Fatalf("output = %#v", output)
	}

	if _, err := Aggregate(2, resources[:1], results); err == nil {
		t.Fatal("Aggregate accepted incomplete backend coverage")
	}

	if _, err := Aggregate(2, resources, results[:1]); err == nil {
		t.Fatal("Aggregate accepted incomplete results")
	}
}

func TestAggregateBackendFailurePrecedesDiagnosticFailure(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(2, testBackendResources(), []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "DiagnosticFailed"},
		{ParticipantIndex: 1, ExecutionState: "Error", Verdict: "Inconclusive", Reason: "BackendFailed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "BackendFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestAggregateRetainsDiagnosticFailureWhenBackendsSucceed(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(2, testBackendResources(), []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "DiagnosticFailed"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if output.Verdict != "Fail" || output.Reason != "ParticipantCertificationFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestAggregateBoundsResultLogMetadataAtMaximumParticipants(t *testing.T) {
	t.Parallel()

	resources := make([]signalv1.BackendResourceReference, MaxParticipants)

	results := make([]Result, MaxParticipants)
	for index := range MaxParticipants {
		resources[index] = signalv1.BackendResourceReference{
			APIVersion:         "kubeflow.org/v2beta1",
			Kind:               "MPIJob",
			Namespace:          "project-signal",
			Name:               fmt.Sprintf("run-p%d", index),
			UID:                types.UID(fmt.Sprintf("uid-%d", index)),
			ParticipantIndexes: []int32{int32(index)},
		}
		results[index] = Result{
			ParticipantIndex: index,
			ExecutionState:   "Succeeded",
			Verdict:          "Pass",
			Reason:           "Healthy",
			ResultLog: &ResultLogMetadata{
				Current:        ResultLogSourceMetadata{Status: "valid", Bytes: 128},
				Previous:       ResultLogSourceMetadata{Status: "unavailable"},
				SelectedSource: "current",
			},
		}
	}

	output, err := Aggregate(MaxParticipants, resources, results)
	if err != nil {
		t.Fatal(err)
	}

	if len(output.Measurements) > MaxMeasurementsBytes {
		t.Fatalf("measurements size = %d", len(output.Measurements))
	}
}

func TestAggregateRejectsDuplicateIdentityAndOversizedMeasurements(t *testing.T) {
	t.Parallel()

	resources := testBackendResources()
	resources[1] = resources[0]
	resources[1].ParticipantIndexes = []int32{1}

	results := []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	}
	if _, err := Aggregate(2, resources, results); err == nil {
		t.Fatal("Aggregate accepted duplicate backend identity")
	}

	resources = testBackendResources()

	results[1].Measurements = strings.Repeat("x", MaxMeasurementsBytes+1)
	if _, err := Aggregate(2, resources, results); err == nil {
		t.Fatal("Aggregate accepted oversized measurements")
	}
}

func TestParticipantBounds(t *testing.T) {
	t.Parallel()

	if _, err := ParseParticipants("[]", 0, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted zero participants")
	}

	if _, err := ParseParticipants("[]", MaxParticipants+1, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted too many participants")
	}
}

func TestBackendResourceRequiresMatchingParticipantLabel(t *testing.T) {
	t.Parallel()

	resource := ordinaryPod(
		testConfig(),
		testParticipants()[0],
		nil,
		nil,
		signalv1.WorkloadPolicy{},
		testWorkflowOwner(),
		900,
		0,
	)
	resource.UID = types.UID("pod-uid")

	content, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(content, &object); err != nil {
		t.Fatal(err)
	}

	unstructuredResource := &unstructured.Unstructured{Object: object}
	if _, err := BackendResource(unstructuredResource, 1); err == nil {
		t.Fatal("BackendResource accepted mismatched participant label")
	}
}

func TestParseResultLogUsesLastCompleteRecord(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logContent := "noise\n" + resultPrefix +
		encode("Pass") + " " +
		encode("Healthy") + " " +
		encode("temperature=42") + " 0\n"

	result, err := ParseResultLog(logContent, 1)
	if err != nil {
		t.Fatal(err)
	}

	if result.ParticipantIndex != 1 ||
		result.Verdict != "Pass" ||
		result.Measurements != "temperature=42" {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseResultLogAllowsEmptyMeasurements(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logContent := resultPrefix + encode("Pass") + " " + encode("Healthy") + " - 0\n"

	result, err := ParseResultLog(logContent, 0)
	if err != nil {
		t.Fatal(err)
	}

	if result.Measurements != "" {
		t.Fatalf("measurements = %q", result.Measurements)
	}
}

func TestParseResultLogAcceptsInconclusiveExitTwo(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logContent := resultPrefix +
		encode("Inconclusive") + " " +
		encode("DiagnosticUnavailable") + " " +
		encode("device_state=unknown") + " 2\n"

	result, err := ParseResultLog(logContent, 0)
	if err != nil {
		t.Fatal(err)
	}

	if result.ExecutionState != "Succeeded" ||
		result.Verdict != "Inconclusive" ||
		result.ExitCode != 2 {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseResultLogRejectsContradictoryVerdictExitCode(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}

	for name, test := range map[string]struct {
		verdict  string
		exitCode string
	}{
		"Pass with diagnostic failure":         {verdict: "Pass", exitCode: "1"},
		"Fail with successful process":         {verdict: "Fail", exitCode: "0"},
		"Inconclusive with successful process": {verdict: "Inconclusive", exitCode: "0"},
		"unexpected exit code":                 {verdict: "Inconclusive", exitCode: "3"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logContent := resultPrefix +
				encode(test.verdict) + " " +
				encode("SyntheticReason") + " - " +
				test.exitCode + "\n"
			if _, err := ParseResultLog(logContent, 0); err == nil {
				t.Fatal("ParseResultLog accepted contradictory result")
			}
		})
	}
}

func TestParseResultLogRejectsMalformedAndMissingResults(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}

	for name, logContent := range map[string]string{
		"malformed verdict": resultPrefix + "%%% " + encode("Reason") + " - 2\n",
		"missing verdict":   resultPrefix + "- " + encode("Reason") + " - 2\n",
		"missing reason":    resultPrefix + encode("Inconclusive") + " - - 2\n",
		"missing record":    "diagnostic output only\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseResultLog(logContent, 0); err == nil {
				t.Fatal("ParseResultLog accepted invalid result")
			}
		})
	}
}

func TestResultCommandClearsReservedFilesAndReportsExitCode(t *testing.T) {
	t.Parallel()

	command := resultCommand()
	if len(command) != 3 {
		t.Fatalf("resultCommand() = %#v", command)
	}

	for _, required := range []string{
		"rm -f /results/verdict /results/reason /results/measurements.txt",
		`printf 'PROJECT_SIGNAL_RESULTS_V1 prepared\n'`,
		`printf 'PROJECT_SIGNAL_RESULTS_V1 verdict=%s reason=%s measurements=%s\n'`,
		`printf 'PROJECT_SIGNAL_RESULT_V2 %s %s %s %d\n'`,
		`exit "$status"`,
	} {
		if !strings.Contains(command[2], required) {
			t.Fatalf("result command does not contain %q:\n%s", required, command[2])
		}
	}

	for _, forbidden := range []string{
		"ScriptDidNotProduceReason",
		"script_exit_code=",
	} {
		if strings.Contains(command[2], forbidden) {
			t.Fatalf("result command still contains fallback %q:\n%s", forbidden, command[2])
		}
	}
}

func testConfig() Config {
	participantsJSON, err := json.Marshal(testParticipants())
	if err != nil {
		panic(err)
	}

	return Config{
		Namespace:              "project-signal",
		WorkflowName:           "run-example-a1",
		RunName:                "run-example",
		RunUID:                 types.UID("run-uid"),
		ParticipantsJSON:       string(participantsJSON),
		ParticipantCount:       2,
		RequestNodeName:        "node-a",
		LegacyParticipants:     "node-a,node-b",
		ScriptConfigMap:        "run-example-scripts",
		ScriptKey:              "check.sh",
		ScriptDigest:           "sha256:" + strings.Repeat("b", 64),
		RunnerImage:            "example.invalid/runner@sha256:" + strings.Repeat("a", 64),
		BackendAPIVersion:      "kubeflow.org/v2beta1",
		BackendKind:            "MPIJob",
		BackendDeadlineSeconds: 540,
		Queue:                  "certification",
		TolerationsJSON:        `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`,
		ScriptEnvironmentJSON:  `{"CHECK_MODE":"quick"}`,
		GPUResourceCount:       "1",
		Attempt:                1,
		BackoffLimit:           0,
		TTLSeconds:             3600,
	}
}

func testParticipants() []signalv1.Participant {
	return []signalv1.Participant{
		{Index: 0, NodeName: "node-a", NodeUID: types.UID("uid-a")},
		{Index: 1, NodeName: "node-b", NodeUID: types.UID("uid-b")},
	}
}

func environmentValue(environment []corev1.EnvVar, name string) string {
	for _, variable := range environment {
		if variable.Name == name {
			return variable.Value
		}
	}

	return ""
}

func testBackendResources() []signalv1.BackendResourceReference {
	return []signalv1.BackendResourceReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Namespace:          "project-signal",
			Name:               "run-example-p0",
			UID:                types.UID("uid-0"),
			ParticipantIndexes: []int32{0},
		},
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Namespace:          "project-signal",
			Name:               "run-example-p1",
			UID:                types.UID("uid-1"),
			ParticipantIndexes: []int32{1},
		},
	}
}

func testWorkflowOwner() metav1.OwnerReference {
	controller := true

	return metav1.OwnerReference{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Workflow",
		Name:       "run-example-a1",
		UID:        types.UID("workflow-uid"),
		Controller: &controller,
	}
}

func quantity(value string) *resource.Quantity {
	result := resource.MustParse(value)

	return &result
}
