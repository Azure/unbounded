// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func testBuildConfig() *ImageBuildConfig {
	pinned := "example.azurecr.io/runtime@sha256:" + strings.Repeat("a", 64)
	return &ImageBuildConfig{Image: pinned, Base: pinned, Repository: "demo/generated", ServiceAccount: "gantry-image-builder", MaxConcurrent: 1}
}

func TestBuildAPICreatesOnlyBoundedWorkloadIdentityJobs(t *testing.T) {
	client := fake.NewSimpleClientset()

	server := testServer(client, testImage())
	if response := apiRequest(server, http.MethodPost, "/api/builds", `{"layers":2,"layerMB":3}`); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured builder accepted request: %s", response.Body)
	}

	server.cfg.Builder = testBuildConfig()
	if err := server.cfg.Builder.Validate(server.cfg.Registry); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{"layers":0,"layerMB":1}`, `{"layers":100,"layerMB":1001}`, `{"layers":101,"layerMB":1}`, `{"layers":2.5,"layerMB":1}`, `{"layers":2,"layerMB":1,"repository":"other"}`, `{"layers":2,"layerMB":1,"streaming":"true"}`, `{"layers":2,"layerMB":1} {}`} {
		if response := apiRequest(server, http.MethodPost, "/api/builds", body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request %s: %d %s", body, response.Code, response.Body)
		}
	}

	response := apiRequest(server, http.MethodPost, "/api/builds", `{"layers":2,"layerMB":3}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create build: %d %s", response.Code, response.Body)
	}

	var build ImageBuild
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	job, err := client.BatchV1().Jobs("demo").Get(context.Background(), build.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	pod := job.Spec.Template

	container := pod.Spec.Containers[0]
	if !buildName.MatchString(job.Name) || *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != int64(ImageBuildTimeout/time.Second) ||
		pod.Spec.ServiceAccountName != "gantry-image-builder" || pod.Labels["azure.workload.identity/use"] != "true" ||
		pod.Spec.NodeSelector["kubernetes.azure.com/mode"] != "system" || !*pod.Spec.SecurityContext.RunAsNonRoot ||
		!*container.SecurityContext.ReadOnlyRootFilesystem || container.Image != server.cfg.Builder.Image || container.Command[1] != "build-image" {
		t.Fatalf("unexpected builder Job: %+v", job.Spec)
	}

	var spec BuildSpec
	if err := json.Unmarshal([]byte(container.Env[0].Value), &spec); err != nil {
		t.Fatal(err)
	}

	if spec.BuildRequest != (BuildRequest{Layers: 2, LayerMB: 3}) || spec.Registry != server.cfg.Registry || spec.Namespace != "demo" || spec.Repository != "demo/generated" {
		t.Fatalf("untrusted build specification: %+v", spec)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "daemonsets" {
			t.Fatal("HTTP create path started a pull before the builder published its image")
		}
	}

	restarted := testServer(client, testImage())

	restarted.cfg.Builder = testBuildConfig()
	if response := apiRequest(restarted, http.MethodPost, "/api/builds", `{"layers":1,"layerMB":1}`); response.Code != http.StatusConflict {
		t.Fatalf("restart lost active-build limit: %d", response.Code)
	}
}

func TestBuildAPIPreservesStreamingOptIn(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			client := fake.NewSimpleClientset()
			server := testServer(client, testImage())
			server.cfg.Builder = testBuildConfig()

			response := apiRequest(server, http.MethodPost, "/api/builds", fmt.Sprintf(`{"layers":1,"layerMB":1,"streaming":%t}`, streaming))
			if response.Code != http.StatusCreated {
				t.Fatalf("create build: %d %s", response.Code, response.Body)
			}

			var build ImageBuild
			if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
				t.Fatal(err)
			}

			job, err := client.BatchV1().Jobs("demo").Get(context.Background(), build.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			var spec, saved BuildSpec
			if err := json.Unmarshal([]byte(job.Spec.Template.Spec.Containers[0].Env[0].Value), &spec); err != nil {
				t.Fatal(err)
			}

			if err := json.Unmarshal([]byte(job.Annotations[buildAnnotation]), &saved); err != nil {
				t.Fatal(err)
			}

			if build.Request.Streaming != streaming || spec.Streaming != streaming || saved.Streaming != streaming {
				t.Fatal("streaming selection was not preserved in response and durable Job")
			}
		})
	}
}

func TestLargeBuildJobsReserveOnePackagedLayer(t *testing.T) {
	for _, request := range []BuildRequest{{Layers: 100, LayerMB: 1}, {Layers: 100, LayerMB: 1000}, {Layers: 1, LayerMB: 100_000}} {
		t.Run(fmt.Sprintf("%dx%dMB", request.Layers, request.LayerMB), func(t *testing.T) {
			client := fake.NewSimpleClientset()
			server := testServer(client, testImage())
			server.cfg.Builder = testBuildConfig()

			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}

			response := apiRequest(server, http.MethodPost, "/api/builds", string(body))
			if response.Code != http.StatusCreated {
				t.Fatalf("large request rejected: %d %s", response.Code, response.Body)
			}

			jobs, err := client.BatchV1().Jobs("demo").List(context.Background(), metav1.ListOptions{})
			if err != nil || len(jobs.Items) != 1 {
				t.Fatalf("expected one build Job: %v, %v", jobs, err)
			}

			pod := jobs.Items[0].Spec.Template.Spec
			resources := pod.Containers[0].Resources
			reserved := resources.Requests.StorageEphemeral().Value()
			limit := resources.Limits.StorageEphemeral().Value()

			payload := int64(request.LayerMB) * payloadMB
			if reserved < payload+payload/100 || reserved > payload+payload/100+256*1024*1024 ||
				limit != reserved || pod.Volumes[0].EmptyDir.SizeLimit.Value() != limit {
				t.Fatalf("storage must cover one packaged layer, not the entire image: reserved=%d limit=%d payload=%d", reserved, limit, payload)
			}
		})
	}
}

func TestBuildStatusRejectsOCIWorkloadForStreamingRequest(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := testServer(client, testImage())
	server.cfg.Builder = testBuildConfig()

	response := apiRequest(server, http.MethodPost, "/api/builds", `{"layers":1,"layerMB":1,"streaming":true}`)

	var build ImageBuild
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("create streaming build: %d %v", response.Code, err)
	}

	image := testImage()
	image.Repository, image.Tag = "demo/generated", build.Name
	image.Reference = server.cfg.Registry + "/" + image.Repository + "@" + image.Digest
	image.Streaming = false

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	workload.Labels[buildLabel] = build.Name
	if _, err := client.AppsV1().DaemonSets("demo").Create(context.Background(), workload, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	response = apiRequest(server, http.MethodGet, "/api/builds/"+build.Name, "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("streaming request accepted an OCI workload: %d %s", response.Code, response.Body)
	}
}

func TestBuildAPIConcurrentRequestsCreateOneJob(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := testServer(client, testImage())
	server.cfg.Builder = testBuildConfig()

	var group sync.WaitGroup

	statuses := make(chan int, 4)

	for range 4 {
		group.Go(func() {
			statuses <- apiRequest(server, http.MethodPost, "/api/builds", `{"layers":1,"layerMB":1}`).Code
		})
	}

	group.Wait()
	close(statuses)

	created := 0

	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected concurrent build status %d", status)
		}
	}

	if created != 1 {
		t.Fatalf("created %d concurrent builds", created)
	}
}

func TestBuildStatusResumesOwnedDeploymentAndReportsFailedJobs(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := testServer(client, testImage())
	server.cfg.Builder = testBuildConfig()
	response := apiRequest(server, http.MethodPost, "/api/builds", `{"layers":2,"layerMB":1}`)

	var build ImageBuild
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	job, err := client.BatchV1().Jobs("demo").Get(context.Background(), build.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	job.UID = types.UID("owned-job")

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Build exceeded its deadline"}}
	if _, err := client.BatchV1().Jobs("demo").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	response = apiRequest(server, http.MethodGet, "/api/builds/"+job.Name, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read failed build: %d %s", response.Code, response.Body)
	}

	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	if build.Progress.Stage != "failed" || !strings.Contains(build.Progress.Message, "DeadlineExceeded") {
		t.Fatalf("failure was hidden: %+v", build)
	}

	response = apiRequest(server, http.MethodPost, "/api/builds", `{"layers":1,"layerMB":1}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("finished build retained concurrency slot: %d %s", response.Code, response.Body)
	}

	image := testImage()
	image.Repository = "demo/generated"
	image.Tag = job.Name
	image.Reference = server.cfg.Registry + "/" + image.Repository + "@" + image.Digest

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	workload.Labels[buildLabel] = job.Name
	if _, err := client.AppsV1().DaemonSets("demo").Create(context.Background(), workload, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	restarted := testServer(client, testImage())

	response = apiRequest(restarted, http.MethodGet, "/api/builds/"+job.Name, "")
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || build.Deployment == nil || build.Deployment.Image.Reference != image.Reference || build.Progress.Stage != "failed" {
		t.Fatalf("persisted workload or Job failure not rediscovered: %d %s", response.Code, response.Body)
	}

	job.Labels = nil
	if _, err := client.BatchV1().Jobs("demo").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if response := apiRequest(server, http.MethodGet, "/api/builds/"+job.Name, ""); response.Code == http.StatusOK {
		t.Fatal("foreign Job was read")
	}
}

func TestCompletedBuildDoesNotClaimMissingWorkloadWasStarted(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := testServer(client, testImage())
	server.cfg.Builder = testBuildConfig()
	response := apiRequest(server, http.MethodPost, "/api/builds", `{"layers":1,"layerMB":1}`)

	var build ImageBuild
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	job, err := client.BatchV1().Jobs("demo").Get(context.Background(), build.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	finished := metav1.NewTime(time.Now().Add(-time.Minute))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

	job.Status.CompletionTime = &finished
	if _, err := client.BatchV1().Jobs("demo").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	response = apiRequest(server, http.MethodGet, "/api/builds/"+job.Name, "")
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || build.Progress.Stage != "failed" || build.Deployment != nil {
		t.Fatalf("missing workload reported as success: %d %s", response.Code, response.Body)
	}
}

func TestBuildLogsExposeFailureCauseWithoutSignedQueries(t *testing.T) {
	for _, line := range []string{
		`{"stage":"failed","message":"publish failed: Get \"https://example.data.azurecr.io/blob?sig=private-test-marker\": i/o timeout"}`,
		`2026/11/10 02:20:55 ERROR Gantry UI stopped error="read runtime base: Get \"https://example.data.azurecr.io/blob?sig=private-test-marker\": i/o timeout"`,
	} {
		progress, ok := parseBuildLog([]byte(line))
		if !ok || progress.Stage != "failed" || !strings.Contains(progress.Message, "i/o timeout") ||
			strings.Contains(progress.Message, "private-test-marker") || strings.Contains(progress.Message, "?sig=") {
			t.Fatalf("invalid or unredacted diagnostic: %+v", progress)
		}
	}

	if _, ok := parseBuildLog([]byte("unrelated log line")); ok {
		t.Fatal("unrelated logs became build progress")
	}

	if progress, ok := parseBuildLog([]byte(`{"stage":"pushing","layer":3,"layers":10,"bytes":42}`)); !ok ||
		progress.Stage != "pushing" || progress.Layer != 3 || progress.Bytes != 42 {
		t.Fatalf("normal progress was lost: %+v", progress)
	}
}
