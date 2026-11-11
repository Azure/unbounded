// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func readStreamFrame(t *testing.T, scanner *bufio.Scanner) (string, []byte) {
	t.Helper()

	var event string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}

		if strings.HasPrefix(line, "data: ") {
			return event, []byte(strings.TrimPrefix(line, "data: "))
		}
	}

	t.Fatalf("stream ended: %v", scanner.Err())

	return "", nil
}

func TestStreamPublishesFastWorkerBeforeSlowWorkerAndSharesCollection(t *testing.T) {
	image := testImage()

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	workload.UID = types.UID("owned-workload")
	client := fake.NewSimpleClientset(workload)

	for index, name := range []string{"fast", "slow"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"kubernetes.azure.com/agentpool": "flexpool", "kubernetes.azure.com/nodepool-type": "FlexNodes",
			"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64",
		}}}
		agent := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "gantry-" + name, Namespace: "unbounded-system", Labels: map[string]string{"app.kubernetes.io/name": "gantry"}},
			Spec:       corev1.PodSpec{NodeName: name, Containers: []corev1.Container{{Name: "gantry"}}},
			Status:     corev1.PodStatus{PodIP: []string{"10.0.0.1", "10.0.0.2"}[index]},
		}

		if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}

		if _, err := client.CoreV1().Pods("unbounded-system").Create(context.Background(), agent, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	fast := make(chan struct{})
	canceled := make(chan struct{}, 1)

	var requests atomic.Int32

	server := testServer(client, image)
	server.cfg.Interval = time.Hour
	server.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)

		if request.URL.Hostname() == "10.0.0.2" {
			<-request.Context().Done()

			canceled <- struct{}{}

			return nil, request.Context().Err()
		}

		select {
		case <-fast:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"blobs":[{"digest":"` + image.Layers[0].Digest + `","available":12,"complete":false}]}`))}, nil
	})

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connect := func() (*http.Response, *bufio.Scanner) {
		t.Helper()

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/deployments/"+image.ID+"/stream", nil)
		if err != nil {
			t.Fatal(err)
		}

		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
		})

		scanner := bufio.NewScanner(response.Body)
		if event, _ := readStreamFrame(t, scanner); event != "snapshot" {
			t.Fatalf("expected initial snapshot, got %q", event)
		}

		return response, scanner
	}
	first, firstScanner := connect()
	second, secondScanner := connect()

	close(fast)

	started := time.Now()

	for _, scanner := range []*bufio.Scanner{firstScanner, secondScanner} {
		event, body := readStreamFrame(t, scanner)
		if event != "node-updates" {
			t.Fatalf("expected worker update, got %q", event)
		}

		var update nodeUpdates
		if err := json.Unmarshal(body, &update); err != nil {
			t.Fatal(err)
		}

		if len(update.Nodes) != 1 || update.Nodes[0].Name != "fast" || update.Nodes[0].Available == nil || *update.Nodes[0].Available != 12 ||
			len(update.Nodes[0].Layers) != 0 {
			t.Fatalf("expected only the fast worker's grid observation: %s", body)
		}
	}

	if time.Since(started) > time.Second {
		t.Fatal("fast progress waited for the blocked worker")
	}

	if requests.Load() != 2 {
		t.Fatalf("viewers duplicated collection: %d agent requests", requests.Load())
	}

	response := apiRequest(server, http.MethodGet, "/api/deployments/"+image.ID+"/nodes/fast", "")

	var node Node
	if err := json.Unmarshal(response.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || node.Available == nil || *node.Available != 12 || len(node.Layers) != 1 {
		t.Fatalf("partial node details unavailable: %d %s", response.Code, response.Body)
	}

	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-canceled:
		t.Fatal("closing one viewer canceled the shared collection")
	case <-time.After(50 * time.Millisecond):
	}

	if err := second.Body.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-canceled:
		t.Fatal("closing the last viewer lost full-run collection")
	case <-time.After(50 * time.Millisecond):
	}

	cached := server.snapshots[image.ID]
	cached.mu.Lock()
	cached.cancel()
	cached.mu.Unlock()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("explicit collector cancellation did not stop agent requests")
	}
}

func TestStreamReportsCollectionFailureInsteadOfAnEmptySuccess(t *testing.T) {
	image := testImage()

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	server := testServer(fake.NewSimpleClientset(workload), image)
	cached := &snapshotCache{err: errors.New("node inventory unavailable")}
	// Keep the injected failure stable while the shared collector waits.
	cached.collectMu.Lock()

	server.snapshots[image.ID] = cached
	defer func() {
		cached.mu.Lock()
		if cached.cancel != nil {
			cached.cancel()
		}
		cached.mu.Unlock()
	}()
	defer cached.collectMu.Unlock()

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/deployments/"+image.ID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	event, body := readStreamFrame(t, bufio.NewScanner(response.Body))
	if event != "status-error" || !strings.Contains(string(body), "node inventory unavailable") {
		t.Fatalf("collection failure was hidden: %q %s", event, body)
	}
}

func TestStreamReconnectReturnsEntireRunHistory(t *testing.T) {
	image := testImage()
	start := time.Now().UTC().Add(-3 * time.Minute)

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	server := testServer(fake.NewSimpleClientset(workload), image)
	cached := &snapshotCache{
		ctx: context.Background(),
		snapshot: Snapshot{
			Deployment: Deployment{ID: image.ID, UID: "run", Created: start},
			ObservedAt: time.Now().UTC(),
			Nodes:      []*Node{{Name: "worker", Phase: "ready"}},
		},
		historyUID: "run",
		history: RunHistory{
			Start: start, ObservedFrom: start, OriginBytes: 1234, PeerBytes: 5678,
			Points: []RunPoint{
				{At: start, Total: 1},
				{At: start.Add(time.Minute), Ready: 1, Total: 1},
				{At: time.Now().UTC(), Ready: 1, Total: 1, OriginBytes: 1234, PeerBytes: 5678},
			},
		},
	}
	server.snapshots[image.ID] = cached

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/deployments/"+image.ID+"/stream", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		response, err := httpServer.Client().Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		event, body := readStreamFrame(t, bufio.NewScanner(response.Body))

		var snapshot Snapshot

		err = json.Unmarshal(body, &snapshot)
		_ = response.Body.Close()

		cancel()

		if err != nil || event != "snapshot" || snapshot.History == nil || len(snapshot.History.Points) != 3 ||
			!snapshot.History.Points[0].At.Equal(start) || snapshot.History.OriginBytes != 1234 || snapshot.History.PeerBytes != 5678 {
			t.Fatalf("reconnection lost full-run history: %s %v", body, err)
		}
	}
}

func TestRunCollectionStopsWhenDeploymentIsDeleted(t *testing.T) {
	server := testServer(fake.NewSimpleClientset(), testImage())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cached := &snapshotCache{ctx: ctx, cancel: cancel}
	finished := make(chan struct{})

	go func() {
		server.runCollection(ctx, testImage().ID, cached)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("collector continued for a deleted deployment")
	}

	if ctx.Err() == nil || cached.err == nil {
		t.Fatal("deleted deployment did not cancel its collector and preserve the error")
	}
}
