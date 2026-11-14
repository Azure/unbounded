// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/progress"
)

func TestCollectionKeepsCompleteAgentRosterAndBandwidthBaseline(t *testing.T) {
	image := testImage()

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleClientset(workload)

	for index, name := range []string{"worker-a", "worker-b", "native"} {
		if name != "native" {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				"kubernetes.azure.com/agentpool": "flexpool", "kubernetes.azure.com/nodepool-type": "FlexNodes",
				"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64",
			}}}
			if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}

		agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gantry-" + name, Namespace: "unbounded-system", UID: types.UID(name), Labels: map[string]string{"app.kubernetes.io/name": "gantry"}}, Spec: corev1.PodSpec{NodeName: name, Containers: []corev1.Container{{Name: "gantry"}}}, Status: corev1.PodStatus{PodIP: "10.0.0." + strconv.Itoa(index+1)}}
		if _, err := client.CoreV1().Pods("unbounded-system").Create(context.Background(), agent, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	server := testServer(client, image)
	counts := map[string]int{}

	var mu sync.Mutex

	server.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		counts[request.URL.Hostname()]++
		count := counts[request.URL.Hostname()]
		mu.Unlock()

		response := progress.Response{Traffic: progress.Traffic{OriginBytes: float64(count * 100), PeerBytes: float64(count * 200)}, Activity: &progress.Activity{OriginActive: 1, PeerActive: 2}}

		if request.URL.Hostname() == "10.0.0.3" {
			if request.URL.RawQuery != "" {
				t.Error("native activity probe unexpectedly requested image blobs")
			}
		} else {
			response.Blobs = []containerdstore.BlobProgress{{Digest: image.Layers[0].Digest, Available: 100, Total: 100, Complete: true}}
		}

		body, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})

	cached, err := server.snapshotEntry(image.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := server.refreshSnapshot(context.Background(), image.ID, cached, true); err != nil {
		t.Fatal(err)
	}

	first := server.clusterActivity(time.Now())
	if first.Agents != 3 || first.Reporting != 3 || first.RateReporting != 0 || *first.OriginActive != 3 || *first.PeerActive != 6 {
		t.Fatalf("collection published a partial roster or dropped the native agent: %+v", first)
	}

	time.Sleep(275 * time.Millisecond)

	if _, err := server.refreshSnapshot(context.Background(), image.ID, cached, true); err != nil {
		t.Fatal(err)
	}

	second := server.clusterActivity(time.Now())
	if !second.Complete || second.RateReporting != 3 || second.OriginRate == nil || *second.OriginRate <= 0 || second.PeerRate == nil || *second.PeerRate <= 0 {
		t.Fatalf("refresh discarded the previous counter baseline: %+v", second)
	}
}
