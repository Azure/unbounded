// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/progress"
)

type fixedCatalog struct{ catalog Catalog }

func (c fixedCatalog) Catalog(context.Context) (Catalog, error) { return c.catalog, nil }

type countingCatalog struct {
	catalog Catalog
	calls   int
}

func (c *countingCatalog) Catalog(context.Context) (Catalog, error) {
	c.calls++
	return c.catalog, nil
}

func TestNodeDetailDoesNotWaitForClusterRefresh(t *testing.T) {
	client := fake.NewSimpleClientset()
	image := testImage()
	server := testServer(client, image)
	observed := time.Now().Add(-time.Minute).UTC()
	cache := &snapshotCache{
		at: observed,
		snapshot: Snapshot{
			ObservedAt: observed,
			Nodes: []*Node{{
				Name: "worker", Available: ptr.To(int64(12)), UpdatedAt: &observed,
				Layers: []LayerProgress{{Layer: image.Layers[0], Available: ptr.To(int64(12))}},
			}},
		},
	}
	server.snapshots[image.ID] = cache
	cache.collectMu.Lock()
	defer cache.collectMu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- apiRequest(server, http.MethodGet, "/api/deployments/"+image.ID+"/nodes/worker", "")
	}()

	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("node detail: %d %s", response.Code, response.Body)
		}

		var node Node
		if err := json.Unmarshal(response.Body.Bytes(), &node); err != nil {
			t.Fatal(err)
		}

		if node.Available == nil || *node.Available != 12 || len(node.Layers) != 1 ||
			node.UpdatedAt == nil || !node.UpdatedAt.Equal(observed) {
			t.Fatalf("cached observations changed: %+v", node)
		}

		if len(client.Actions()) != 0 {
			t.Fatal("reading collected layer details should not issue new Kubernetes requests")
		}
	case <-time.After(time.Second):
		t.Fatal("node selection waited for the cluster refresh")
	}
}

func TestCatalogRefreshBypassesCachedImages(t *testing.T) {
	server := testServer(fake.NewSimpleClientset(), testImage())
	catalog := &countingCatalog{catalog: Catalog{Images: []Image{testImage()}}}
	server.cfg.Catalog = catalog

	for _, path := range []string{"/api/images", "/api/images", "/api/images?refresh=1"} {
		response := apiRequest(server, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("catalog: %d %s", response.Code, response.Body)
		}
	}

	if catalog.calls != 2 {
		t.Fatalf("expected initial and explicit refresh calls, got %d", catalog.calls)
	}
}

func testImage() Image {
	d := "sha256:" + strings.Repeat("a", 64)

	return Image{
		ID: strings.Repeat("a", 20), Repository: "smoke-test/regular", Tag: "test",
		Digest: d, Reference: "example.azurecr.io/smoke-test/regular@" + d,
		Architecture: "amd64", OS: "linux", Size: 100,
		Layers: []Layer{{Digest: "sha256:" + strings.Repeat("b", 64), Size: 100}},
	}
}

func testServer(client *fake.Clientset, image Image) *Server {
	return New(Config{
		Client: client, Catalog: fixedCatalog{Catalog{Registry: "example.azurecr.io", Images: []Image{image}}},
		Registry: "example.azurecr.io", Namespace: "demo", NodePool: "flexpool", Architecture: "amd64",
		GantryNamespace: "unbounded-system", DirectAgents: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func apiRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	return response
}

func TestDeploymentClickIsIdempotentAndPinnedToFlexWorkers(t *testing.T) {
	client := fake.NewSimpleClientset()
	image := testImage()

	server := testServer(client, image)
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		response := apiRequest(server, http.MethodPost, "/api/deployments", `{"imageId":"`+image.ID+`"}`)
		if response.Code != status {
			t.Fatalf("deploy: %d %s", response.Code, response.Body)
		}
	}

	list, err := client.AppsV1().DaemonSets("demo").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(list.Items) != 1 {
		t.Fatalf("repeated image selection created %d DaemonSets", len(list.Items))
	}

	spec := list.Items[0].Spec.Template.Spec
	if spec.NodeSelector["kubernetes.azure.com/agentpool"] != "flexpool" ||
		spec.NodeSelector["kubernetes.azure.com/nodepool-type"] != "FlexNodes" ||
		spec.NodeSelector["kubernetes.io/arch"] != "amd64" ||
		spec.Containers[0].Image != image.Reference || *spec.AutomountServiceAccountToken {
		t.Fatalf("unexpected demo workload: %+v", spec)
	}
	// A new server can reconstruct the deployment without its old memory.
	resumed := apiRequest(testServer(client, image), http.MethodGet, "/api/deployments", "")

	var deployments []Deployment
	if err := json.Unmarshal(resumed.Body.Bytes(), &deployments); err != nil {
		t.Fatal(err)
	}

	if len(deployments) != 1 || deployments[0].Image.Reference != image.Reference {
		t.Fatalf("deployment was not rediscovered: %s", resumed.Body)
	}
}

func TestDeploymentRejectsForeignWorkloadsAndIncompatibleImages(t *testing.T) {
	image := testImage()

	ds, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	ds.Labels = nil
	client := fake.NewSimpleClientset(ds)
	server := testServer(client, image)

	response := apiRequest(server, http.MethodPost, "/api/deployments", `{"imageId":"`+image.ID+`"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("foreign workload: %d %s", response.Code, response.Body)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "update" || action.GetVerb() == "patch" {
			t.Fatal("foreign DaemonSet was modified")
		}
	}

	image.Architecture = "arm64"

	response = apiRequest(testServer(fake.NewSimpleClientset(), image), http.MethodPost, "/api/deployments", `{"imageId":"`+image.ID+`"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("architecture mismatch: %d", response.Code)
	}

	response = apiRequest(server, http.MethodPost, "/api/deployments", `{"imageId":"unknown"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown image: %d", response.Code)
	}
}

func TestLocalWebAppRejectsCrossOriginAndRebindingRequests(t *testing.T) {
	server := testServer(fake.NewSimpleClientset(), testImage())

	for _, tc := range []struct{ host, origin string }{
		{"attacker.example", ""}, {"localhost", "https://attacker.example"}, {"localhost", "null"},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://localhost/api/deployments", strings.NewReader("{}"))
		request.Host = tc.host
		request.Header.Set("Origin", tc.origin)
		request.Header.Set("Content-Type", "application/json")

		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		if response.Code != http.StatusForbidden {
			t.Fatalf("host %q origin %q returned %d", tc.host, tc.origin, response.Code)
		}
	}
}

func TestProgressDoesNotConfuseReadyStreamingPodsWithCompleteImages(t *testing.T) {
	node := &Node{Phase: "ready", Layers: []LayerProgress{{Layer: Layer{Digest: "layer", Size: 100}}}}
	if err := applyProgress(node, progress.Response{Blobs: []containerdstore.BlobProgress{{Digest: "layer", Available: 12}}}); err != nil {
		t.Fatal(err)
	}

	if node.Phase != "ready" || node.Available == nil || *node.Available != 12 {
		t.Fatalf("streaming state was conflated: %+v", node)
	}

	for _, blobs := range [][]containerdstore.BlobProgress{
		{},
		{{Digest: "layer", Available: 101}},
		{{Digest: "layer", Available: 12, Complete: true}},
		{{Digest: "layer"}, {Digest: "layer"}},
	} {
		if err := applyProgress(node, progress.Response{Blobs: blobs}); err == nil {
			t.Fatalf("inconsistent observation accepted: %+v", blobs)
		}
	}
}

func TestInvalidProgressPreservesPreviousLayerObservations(t *testing.T) {
	node := &Node{
		Available: ptr.To(int64(60)),
		Layers: []LayerProgress{
			{Layer: Layer{Digest: "first", Size: 30}, Available: ptr.To(int64(30)), Committed: true},
			{Layer: Layer{Digest: "second", Size: 70}, Available: ptr.To(int64(30))},
		},
	}

	for _, blobs := range [][]containerdstore.BlobProgress{
		{{Digest: "first", Available: 10}},
		{{Digest: "first", Available: 10}, {Digest: "second", Available: 71}},
	} {
		if err := applyProgress(node, progress.Response{Blobs: blobs}); err == nil {
			t.Fatal("invalid progress was accepted")
		}

		if *node.Available != 60 || *node.Layers[0].Available != 30 || !node.Layers[0].Committed ||
			*node.Layers[1].Available != 30 {
			t.Fatalf("invalid response partially replaced previous telemetry: %+v", node)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSnapshotKeepsLifecycleWhenOldAgentLacksByteTelemetry(t *testing.T) {
	image := testImage()

	ds, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	ds.UID = types.UID("daemonset-uid")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{
			"kubernetes.azure.com/agentpool": "flexpool", "kubernetes.azure.com/nodepool-type": "FlexNodes",
			"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux",
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-pod", Namespace: "demo", Labels: map[string]string{deploymentLabel: image.ID},
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}},
		},
		Spec: corev1.PodSpec{NodeName: "worker"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	agent := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-worker", Namespace: "unbounded-system", Labels: map[string]string{"app.kubernetes.io/name": "gantry"}},
		Spec:       corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "gantry"}}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
	}
	server := testServer(fake.NewSimpleClientset(ds, node, pod, agent), image)
	server.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/progress" || request.URL.Query().Get("digest") != image.Layers[0].Digest {
			return nil, errors.New("unexpected agent query")
		}

		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})

	snapshot, err := server.snapshot(context.Background(), image.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].Phase != "ready" ||
		snapshot.Nodes[0].Available != nil || !strings.Contains(snapshot.Nodes[0].TelemetryError, "upgrade required") {
		t.Fatalf("old agent should report unknown bytes but real lifecycle: %+v", snapshot)
	}

	if len(snapshot.grid().Nodes[0].Layers) != 0 || len(snapshot.Nodes[0].Layers) != 1 {
		t.Fatal("grid summaries must omit layer detail without modifying the cached node detail")
	}
	// A spoofed label is not sufficient to attach another workload to the UI.
	pod.OwnerReferences[0].UID = "another-daemonset"
	if ownedPod(pod, snapshot.Deployment) {
		t.Fatal("foreign Pod passed ownership fencing")
	}
}

func TestSnapshotRetainsByteTelemetryAcrossAgentFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available int64
		failure   string
	}{
		{name: "unreachable", available: 37, failure: "transport"},
		{name: "reported zero", available: 0, failure: "transport"},
		{name: "complete", available: 100, failure: "http"},
		{name: "invalid progress", available: 37, failure: "invalid"},
		{name: "missing agent", available: 37, failure: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			image := testImage()

			workload, err := buildDeployment("demo", "flexpool", image)
			if err != nil {
				t.Fatal(err)
			}

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{
					"kubernetes.azure.com/agentpool": "flexpool", "kubernetes.azure.com/nodepool-type": "FlexNodes",
					"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux",
				}},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
			}
			agent := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "gantry-worker", Namespace: "unbounded-system", Labels: map[string]string{"app.kubernetes.io/name": "gantry"}},
				Spec:       corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "gantry"}}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
			}
			client := fake.NewSimpleClientset(workload, node, agent)
			server := testServer(client, image)
			available, requests := tc.available, 0
			failing := false
			server.http.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				if failing {
					switch tc.failure {
					case "transport":
						return nil, context.DeadlineExceeded
					case "http":
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
					}
				}

				value := available
				if failing && tc.failure == "invalid" {
					value = image.Size + 1
				}

				requests++

				observation := progress.Response{
					Blobs: []containerdstore.BlobProgress{{Digest: image.Layers[0].Digest, Available: value, Complete: value == image.Size}},
					Traffic: progress.Traffic{
						PeerBytes: float64(requests * 200), OriginBytes: float64(requests * 100),
					},
				}
				if failing {
					observation.StorageError = "filesystem usage unavailable"
				} else {
					observation.Storage = &progress.Storage{Path: "/data", Total: 100, Used: uint64(available), Available: uint64(100 - available)}
				}

				body, err := json.Marshal(observation)
				if err != nil {
					return nil, err
				}

				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})

			cached, err := server.snapshotEntry(image.ID)
			if err != nil {
				t.Fatal(err)
			}

			refresh := func() *Node {
				t.Helper()

				snapshot, err := server.refreshSnapshot(ctx, image.ID, cached, true)
				if err != nil {
					t.Fatal(err)
				}

				if len(snapshot.Nodes) != 1 {
					t.Fatalf("expected one worker, got %d", len(snapshot.Nodes))
				}

				return snapshot.Nodes[0]
			}

			refresh()

			reported := refresh()
			if reported.Available == nil || *reported.Available != available || reported.PeerRate == nil {
				t.Fatalf("successful observation missing: %+v", reported)
			}

			node.Status.Conditions[0].Status = corev1.ConditionFalse
			if _, err := client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}

			failing = true

			if tc.failure == "missing" {
				if err := client.CoreV1().Pods(agent.Namespace).Delete(ctx, agent.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}

			for range 2 {
				stale := refresh()
				if stale.TelemetryError == "" {
					t.Fatal("collection failure was hidden")
				}

				expected := *reported
				expected.NodeReady = false

				expected.TelemetryError = stale.TelemetryError

				expected.StorageError = stale.StorageError
				if !reflect.DeepEqual(stale, &expected) {
					t.Fatalf("failure replaced byte telemetry or suppressed current node state:\ngot %+v\nwant %+v", stale, expected)
				}

				response := apiRequest(server, http.MethodGet, "/api/deployments/"+image.ID+"/nodes/worker", "")

				var detail Node
				if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
					t.Fatal(err)
				}

				if response.Code != http.StatusOK || !reflect.DeepEqual(&detail, stale) {
					t.Fatalf("node details lost retained telemetry: %d %s", response.Code, response.Body)
				}
			}

			failing, available = false, 12

			if tc.failure == "missing" {
				if _, err := client.CoreV1().Pods(agent.Namespace).Create(ctx, agent, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}

			recovered := refresh()
			if recovered.TelemetryError != "" || recovered.Available == nil || *recovered.Available != available ||
				*recovered.Layers[0].Available != available || recovered.Layers[0].Committed ||
				recovered.UpdatedAt == nil || !recovered.UpdatedAt.After(*reported.UpdatedAt) ||
				recovered.StorageError != "" || recovered.Storage == nil || recovered.Storage.Used != uint64(available) ||
				recovered.StorageUpdatedAt == nil || !recovered.StorageUpdatedAt.After(*reported.StorageUpdatedAt) {
				t.Fatalf("fresh telemetry did not replace retained state: %+v", recovered)
			}
		})
	}
}

func TestStorageFailuresPreserveLastReadingWithoutErasingImageProgress(t *testing.T) {
	observed := time.Now().Add(-time.Minute).UTC()
	storage := &progress.Storage{Path: "/data", Total: 100, Used: 70, Available: 20}
	node := &Node{Available: ptr.To(int64(12)), Storage: storage, StorageUpdatedAt: &observed}

	for _, response := range []progress.Response{
		{},
		{StorageError: "filesystem unavailable"},
		{Storage: &progress.Storage{Total: 0}},
		{Storage: &progress.Storage{Total: 100, Used: 101}},
		{Storage: &progress.Storage{Total: 100, Used: 70, Available: 31}},
	} {
		applyStorage(node, response, time.Now().UTC())

		if node.Storage != storage || !node.StorageUpdatedAt.Equal(observed) || node.StorageError == "" || *node.Available != 12 {
			t.Fatalf("failed storage observation replaced prior state or image bytes: %+v", node)
		}
	}

	fresh := time.Now().UTC()
	applyStorage(node, progress.Response{Storage: &progress.Storage{Path: "/data", Total: 100, Used: 100, Available: 0}}, fresh)

	if node.StorageError != "" || node.Storage.Available != 0 || node.Storage.Used != 100 ||
		!node.StorageUpdatedAt.Equal(fresh) || *node.Available != 12 {
		t.Fatalf("full-disk observation was rejected or erased image bytes: %+v", node)
	}
}

func TestLifecycleReportsPreparationWithoutInventingUnpackStatus(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "worker"}}

	phase, reason := podLifecycle(pod)
	if phase != "preparing" || !strings.Contains(reason, "does not report unpack") {
		t.Fatalf("unobserved unpack stage must stay explicit: %s %s", phase, reason)
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now())},
	}}}
	if phase, _ := podLifecycle(pod); phase != "running" {
		t.Fatalf("running is not necessarily Ready, got %s", phase)
	}

	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	if phase, _ := podLifecycle(pod); phase != "failed" {
		t.Fatalf("pull failure was not surfaced: %s", phase)
	}
}
