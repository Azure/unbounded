// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/gantry/progress"
)

func TestRunHistoryRetainsFullRunAndCountsOnlyObservedDeltas(t *testing.T) {
	server := New(Config{})
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "system", UID: "epoch-one"}, Spec: corev1.PodSpec{NodeName: "worker"}}
	server.setAgentRoster(map[string]*corev1.Pod{"worker": agent})

	cached := &snapshotCache{snapshot: Snapshot{
		Deployment: Deployment{UID: "run-one", Created: start},
		Nodes:      []*Node{{Name: "worker", Phase: "preparing"}},
	}}

	for second := 0; second <= 180; second++ {
		if second == 180 {
			cached.snapshot.Nodes[0].Phase = "ready"
		}

		at := start.Add(time.Duration(second) * time.Second)
		server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{
			OriginBytes: 10000 + float64(second)*10, PeerBytes: 20000 + float64(second)*30,
		}}, at, false)
		server.recordRunHistory(cached, at)
	}

	history := server.recordRunHistory(cached, start.Add(180*time.Second))
	if len(history.Points) != 181 || history.OriginBytes != 1800 || history.PeerBytes != 5400 ||
		history.Points[0].At != start || history.Points[180].Ready != 1 {
		t.Fatalf("full history or run-scoped deltas lost: %+v", history)
	}

	reconnected := server.recordRunHistory(cached, start.Add(180*time.Second+100*time.Millisecond))
	if len(reconnected.Points) != 181 || reconnected.OriginBytes != history.OriginBytes {
		t.Fatal("reconnection duplicated history or counters")
	}

	frozen := server.recordRunHistory(cached, start.Add(240*time.Second))
	if len(frozen.Points) != 181 || frozen.CompletedAt == nil || !frozen.CompletedAt.Equal(start.Add(180*time.Second)) {
		t.Fatal("completed run charts kept updating")
	}

	full := cloneRunHistory(history, 0)

	delta := cloneRunHistory(history, 180)
	if len(full.Points) != 181 || len(delta.Points) != 1 {
		t.Fatal("full and incremental history have incorrect shapes")
	}

	full.Points[0].Ready = 99
	if history.Points[0].Ready == 99 {
		t.Fatal("history serialization shares writable slice storage")
	}
}

func TestRunHistoryCounterResetsGapsAndRunIdentity(t *testing.T) {
	server := New(Config{})
	start := time.Now().UTC()
	agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", UID: "epoch-one"}, Spec: corev1.PodSpec{NodeName: "worker"}}
	server.setAgentRoster(map[string]*corev1.Pod{"worker": agent})

	cached := &snapshotCache{snapshot: Snapshot{Deployment: Deployment{UID: "one", Created: start}}}
	record := func(second int, origin float64, failed bool) *RunHistory {
		at := start.Add(time.Duration(second) * time.Second)
		server.recordActivity(agent, progress.Response{Traffic: progress.Traffic{OriginBytes: origin}}, at, failed)

		return server.recordRunHistory(cached, at)
	}
	record(0, 1000, false)
	record(1, 1100, false)
	record(2, 0, true)

	history := record(20, 1200, false)
	if history.OriginBytes != 200 || history.Points[len(history.Points)-1].Origin != nil {
		t.Fatalf("gap fabricated bandwidth or lost counter deltas: %+v", history)
	}

	history = record(21, 20, false)
	if history.OriginBytes != 200 {
		t.Fatal("counter reset invented traffic")
	}

	history = record(22, 30, false)
	if history.OriginBytes != 210 {
		t.Fatal("counter failed to recover after reset")
	}

	agent.UID = "epoch-two"
	server.setAgentRoster(map[string]*corev1.Pod{"worker": agent})

	history = record(23, 10000, false)
	if history.OriginBytes != 210 {
		t.Fatal("agent replacement counted pre-baseline bytes")
	}

	cached.snapshot.Deployment.UID = "two"

	history = record(24, 10010, false)
	if len(history.Points) != 1 || history.OriginBytes != 0 {
		t.Fatal("new run inherited previous history")
	}
}

func TestRunHistoryMissingTelemetryIsNullNotZero(t *testing.T) {
	server := New(Config{})
	now := time.Now().UTC()
	cached := &snapshotCache{snapshot: Snapshot{Deployment: Deployment{UID: "run", Created: now}}}
	history := server.recordRunHistory(cached, now)

	body, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}

	var decoded RunHistory
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Points[0].Origin != nil {
		t.Fatalf("missing bandwidth not preserved: %s %v", body, err)
	}

	// A failure sample must not manufacture a new rate.
	server.agentRoster = map[string]string{"node": "agent"}
	server.agentActivity = map[string]agentActivity{"node": {
		identity: "agent", at: now, failed: true, originRate: ptr.To(100.0), peerRate: ptr.To(200.0),
	}}

	history = server.recordRunHistory(cached, now.Add(time.Second))
	if history.Points[1].Origin != nil {
		t.Fatal("failed agent produced bandwidth")
	}
}
