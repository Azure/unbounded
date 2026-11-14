// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func testReclaimSpec() ReclaimSpec {
	return ReclaimSpec{
		Namespace: "demo", NodePool: "flexpool", Architecture: "amd64", ServiceAccount: "gantry-reclaimer",
		Image: "example.azurecr.io/gantry-ui@sha256:" + strings.Repeat("a", 64),
	}
}

func TestReclaimRequiresExplicitConfirmationAndConfiguration(t *testing.T) {
	spec := testReclaimSpec()

	for _, request := range []string{`{}`, `{"confirm":"yes"}`, `{"confirm":"DELETE ALL DEMOS","extra":true}`, `{"confirm":"DELETE ALL DEMOS"} {}`} {
		t.Run(request, func(t *testing.T) {
			server := testServer(fake.NewSimpleClientset(), testImage())
			server.cfg.Reclaimer = &ReclaimConfig{Image: spec.Image, ServiceAccount: spec.ServiceAccount}

			response := apiRequest(server, http.MethodPost, "/api/reclaim", request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("unconfirmed deletion accepted: %d %s", response.Code, response.Body)
			}

			for _, action := range server.cfg.Client.(*fake.Clientset).Actions() {
				if action.GetVerb() == "create" || action.GetVerb() == "delete" {
					t.Fatal("invalid confirmation mutated resources")
				}
			}
		})
	}

	server := testServer(fake.NewSimpleClientset(), testImage())
	if response := apiRequest(server, http.MethodPost, "/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Body)
	}
}

func TestReclaimCreatesDurableJobAndBlocksOtherMutations(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := testReclaimSpec()
	server := testServer(client, testImage())
	server.cfg.Reclaimer = &ReclaimConfig{Image: spec.Image, ServiceAccount: spec.ServiceAccount}

	response := apiRequest(server, http.MethodPost, "/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create reclaim: %d %s", response.Code, response.Body)
	}

	jobs, err := client.BatchV1().Jobs("demo").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(jobs.Items) != 1 || jobs.Items[0].Name != reclaimJobName {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}

	job := &jobs.Items[0]
	if _, err := ownedReclaim(job); err != nil {
		t.Fatal(err)
	}

	if job.Spec.Template.Spec.ServiceAccountName != spec.ServiceAccount || len(job.Spec.Template.Spec.Volumes) != 0 ||
		job.Spec.Template.Spec.Containers[0].SecurityContext.Privileged != nil {
		t.Fatal("coordinator must not get node privileges")
	}

	for _, test := range []struct{ path, body string }{
		{"/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`},
		{"/api/deployments", `{"imageId":"` + testImage().ID + `"}`},
		{"/api/builds", `{"layers":1,"layerMB":1}`},
	} {
		server.cfg.Builder = &ImageBuildConfig{MaxConcurrent: 1}

		response := apiRequest(server, http.MethodPost, test.path, test.body)
		if response.Code != http.StatusConflict {
			t.Fatalf("mutation during cleanup %s: %d %s", test.path, response.Code, response.Body)
		}
	}
}

func TestReclaimRefusesActiveBuildsAndForeignJobs(t *testing.T) {
	spec := testReclaimSpec()

	for _, job := range []*batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "active-build", Namespace: "demo", Labels: map[string]string{managerLabel: "gantry-ui", buildLabel: "active-build"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: reclaimJobName, Namespace: "demo"}},
	} {
		client := fake.NewSimpleClientset(job)
		server := testServer(client, testImage())
		server.cfg.Reclaimer = &ReclaimConfig{Image: spec.Image, ServiceAccount: spec.ServiceAccount}

		response := apiRequest(server, http.MethodPost, "/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`)
		if response.Code != http.StatusConflict {
			t.Fatalf("conflict not surfaced: %d %s", response.Code, response.Body)
		}

		for _, action := range client.Actions() {
			if action.GetVerb() == "delete" {
				t.Fatal("foreign resource deleted")
			}
		}
	}
}

func TestReclaimRetryCollectsFinishedJobInBackground(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "finished", true: "already deleting"}[deleting], func(t *testing.T) {
			spec := testReclaimSpec()

			job, err := reclaimCoordinatorJob(spec)
			if err != nil {
				t.Fatal(err)
			}

			job.UID = "previous-cleanup"
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

			if deleting {
				now := metav1.Now()
				job.DeletionTimestamp = &now
			}

			client := fake.NewSimpleClientset(job)
			server := testServer(client, testImage())
			server.cfg.Reclaimer = &ReclaimConfig{Image: spec.Image, ServiceAccount: spec.ServiceAccount}

			response := apiRequest(server, http.MethodPost, "/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`)
			if response.Code != http.StatusConflict || response.Header().Get("Retry-After") != "2" {
				t.Fatalf("expected automatic retry signal: %d %s", response.Code, response.Body)
			}

			deletes := 0

			for _, action := range client.Actions() {
				if action.GetVerb() != "delete" {
					continue
				}

				deletes++

				options := action.(k8stesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || *options.Preconditions.UID != job.UID ||
					options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationBackground {
					t.Fatalf("previous coordinator deletion not fenced/background: %+v", options)
				}
			}

			if deleting && deletes != 0 || !deleting && deletes != 1 {
				t.Fatalf("unexpected delete count: %d", deletes)
			}

			if !deleting {
				response = apiRequest(server, http.MethodPost, "/api/reclaim", `{"confirm":"DELETE ALL DEMOS"}`)
				if response.Code != http.StatusCreated {
					t.Fatalf("retry did not create replacement: %d %s", response.Code, response.Body)
				}
			}
		})
	}
}

func TestReclaimValidatesEntireDemoSetBeforeDeletion(t *testing.T) {
	spec := testReclaimSpec()

	owned, err := buildDeployment("demo", "flexpool", testImage())
	if err != nil {
		t.Fatal(err)
	}

	owned.UID = "valid-demo"
	spoof := owned.DeepCopy()
	spoof.Name = "not-a-dashboard-demo"
	spoof.UID = "spoof-demo"

	coordinator, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	coordinator.UID = "cleanup"
	client := fake.NewSimpleClientset(coordinator, owned, spoof)

	err = RunReclaim(context.Background(), client, spec, func(ReclaimProgress) {})
	if err == nil {
		t.Fatal("invalid demo accepted")
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("deleted before validating entire scope")
		}
	}
}

func TestReclaimDeletesOnlyOwnedDemosWithUIDPreconditions(t *testing.T) {
	spec := testReclaimSpec()

	demo, err := buildDeployment("demo", "flexpool", testImage())
	if err != nil {
		t.Fatal(err)
	}

	demo.UID = "owned-demo"
	other := demo.DeepCopy()
	other.Name = "unrelated-workload"
	other.Labels = nil
	other.UID = "other"

	coordinator, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	coordinator.UID = "cleanup"
	client := fake.NewSimpleClientset(coordinator, demo, other)

	var progress ReclaimProgress
	// No matching nodes should fail explicitly after owner-fenced demo deletion.
	err = RunReclaim(context.Background(), client, spec, func(p ReclaimProgress) { progress = p })
	if err == nil || progress.Stage != "failed" || progress.Deleted != 1 {
		t.Fatalf("unexpected result %+v, %v", progress, err)
	}

	if _, err := client.AppsV1().DaemonSets("demo").Get(context.Background(), other.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated workload removed: %v", err)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() != "delete" {
			continue
		}

		raw, _ := json.Marshal(action)
		if !strings.Contains(string(raw), "owned-demo") {
			t.Fatalf("missing UID fence: %s", raw)
		}
	}
}

func TestReclaimWorkerHasOnlyRequiredHostMountsAndNoKubernetesCredentials(t *testing.T) {
	spec := testReclaimSpec()

	owner, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	owner.UID = "cleanup-uid"
	worker := reclaimWorkerJob(owner, spec, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}, nil)

	pod := worker.Spec.Template.Spec
	if pod.NodeName != "worker" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		len(pod.Containers) != 1 || !ptr.Deref(pod.Containers[0].SecurityContext.Privileged, false) {
		t.Fatal("worker permissions do not match cleanup execution")
	}

	if len(pod.Volumes) != 2 || pod.Volumes[0].HostPath.Path != "/run/containerd" || pod.Volumes[1].HostPath.Path != "/var/lib/gantry" ||
		!pod.Containers[0].VolumeMounts[1].ReadOnly {
		t.Fatal("unexpected host filesystem exposure")
	}

	if len(worker.OwnerReferences) != 1 || worker.OwnerReferences[0].UID != types.UID("cleanup-uid") {
		t.Fatal("worker is not fenced to the coordinator Job")
	}
}

func TestReclaimWaitIgnoresPodsFromOtherOwners(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "demo", OwnerReferences: []metav1.OwnerReference{
		{Kind: "DaemonSet", Name: "same-name", UID: "foreign-owner", Controller: ptr.To(true)},
	}}}
	if err := waitForDemoDeletion(context.Background(), fake.NewSimpleClientset(pod), "demo", []Deployment{{Name: "same-name", UID: "real-owner"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimCoordinatorCompletesOnlyWithValidatedWorkerResult(t *testing.T) {
	spec := testReclaimSpec()

	coordinator, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	coordinator.UID = "coordinator"
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{
		"kubernetes.azure.com/agentpool": "flexpool", "kubernetes.azure.com/nodepool-type": "FlexNodes",
		"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64",
	}}}
	client := fake.NewSimpleClientset(coordinator, node)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		created.Name = "worker-job"

		created.UID = "worker-job-uid"
		if err := client.Tracker().Add(created); err != nil {
			return true, nil, err
		}

		report, err := json.Marshal(ReclaimNodeResult{Node: "worker", Before: 1000, After: 100, ProtectedLeases: 2})
		if err != nil {
			return true, nil, err
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-result", Namespace: "demo", Labels: map[string]string{"job-name": created.Name},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: created.Name, UID: created.UID, Controller: ptr.To(true)}},
			},
			Spec: corev1.PodSpec{NodeName: "worker"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "reclaim-node", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(report)},
			}}}},
		}
		if err := client.Tracker().Add(pod); err != nil {
			return true, nil, err
		}

		return true, created, nil
	})

	var final ReclaimProgress

	if err := RunReclaim(context.Background(), client, spec, func(progress ReclaimProgress) { final = progress }); err != nil {
		t.Fatal(err)
	}

	if final.Stage != "complete" || final.Total != 1 || final.Completed != 1 || final.Reclaimed != 900 || final.ProtectedLeases != 2 {
		t.Fatalf("unexpected completion report: %+v", final)
	}
}

func TestReclaimStatusSurvivesDashboardRestartAndFencesWorkerOwnership(t *testing.T) {
	spec := testReclaimSpec()

	coordinator, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	coordinator.UID = "coordinator"
	worker := reclaimWorkerJob(coordinator, spec, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}, nil)
	worker.Name = "worker-job"
	worker.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	foreign := worker.DeepCopy()
	foreign.Name = "foreign"
	foreign.OwnerReferences[0].UID = "other-coordinator"
	client := fake.NewSimpleClientset(coordinator, worker, foreign)
	server := testServer(client, testImage())

	response := apiRequest(server, http.MethodGet, "/api/reclaim", "")
	if response.Code != http.StatusOK {
		t.Fatal(response.Body)
	}

	var state ReclaimState
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}

	if state.UID != "coordinator" || len(state.Workers) != 1 || state.Workers[0].Node != "worker" || state.Workers[0].Stage != "complete" {
		t.Fatalf("bad restored state: %+v", state)
	}
}
