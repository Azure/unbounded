// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/gantry/progress"
)

func TestClusterActivityCountsEachAgentOnceAndIncludesNativeAgent(t *testing.T) {
	server := testServer(fake.NewSimpleClientset(), testImage())
	worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-agent", Namespace: "system", UID: "worker-uid"}, Spec: corev1.PodSpec{NodeName: "worker"}}
	native := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "native-agent", Namespace: "system", UID: "native-uid"}, Spec: corev1.PodSpec{NodeName: "native"}}
	server.setAgentRoster(map[string]*corev1.Pod{"worker": worker, "native": native})

	start := time.Now().UTC()
	for _, agent := range []*corev1.Pod{worker, native} {
		server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 100, PeerBytes: 200}, Activity: &progress.Activity{OriginActive: 2, PeerActive: 3}}, start, false)
		server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 120, PeerBytes: 260}, Activity: &progress.Activity{OriginActive: 1, PeerActive: 4, OriginFailures: 2, PeerBusy: 6, PeerDialFailures: 4}}, start.Add(2*time.Second), false)
		// Another viewer must not create another agent or double-count its totals.
		server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 120, PeerBytes: 260}, Activity: &progress.Activity{OriginActive: 99}}, start.Add(2100*time.Millisecond), false)
	}

	result := server.clusterActivity(start.Add(2 * time.Second))
	if !result.Complete || result.Agents != 2 || *result.OriginRate != 20 || *result.PeerRate != 60 || *result.OriginActive != 2 || *result.PeerActive != 8 ||
		*result.OriginFailures != 2 || *result.PeerBusy != 6 || *result.PeerDialFailures != 4 {
		t.Fatalf("incorrect source aggregation: %+v", result)
	}
}

func TestClusterActivityDoesNotInventMissingStaleOrRestartedMetrics(t *testing.T) {
	server := testServer(fake.NewSimpleClientset(), testImage())
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", UID: "old"}, Spec: corev1.PodSpec{NodeName: "worker"}}
	server.setAgentRoster(map[string]*corev1.Pod{"worker": agent})

	start := time.Now().UTC()
	server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 100}}, start, false)

	result := server.clusterActivity(start)
	if result.Complete || result.OriginRate != nil || result.OriginActive != nil || result.Reporting != 1 {
		t.Fatalf("missing source metrics became zero: %+v", result)
	}

	server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 50}}, start.Add(time.Second), false)

	result = server.clusterActivity(start.Add(time.Second))
	if result.OriginRate != nil {
		t.Fatal("counter reset produced a bandwidth spike")
	}

	result = server.clusterActivity(start.Add(20 * time.Second))
	if result.Reporting != 0 || result.OriginRate != nil {
		t.Fatal("stale agent remained part of the aggregate")
	}

	agent.UID = types.UID("new")
	server.setAgentRoster(map[string]*corev1.Pod{"worker": agent})
	server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: 1000}, Activity: &progress.Activity{}}, start.Add(21*time.Second), false)

	result = server.clusterActivity(start.Add(21 * time.Second))
	if result.OriginRate != nil || result.OriginActive == nil || *result.OriginActive != 0 {
		t.Fatal("new agent inherited old counters or lost an observed zero gauge")
	}

	server.recordActivity(agent, progress.Response{}, start.Add(22*time.Second), true)

	result = server.clusterActivity(start.Add(22 * time.Second))
	if result.Reporting != 0 || result.OriginActive != nil {
		t.Fatal("unreachable agent silently reported zero activity")
	}
}
