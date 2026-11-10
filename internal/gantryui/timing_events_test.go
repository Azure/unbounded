// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestTimingEventsRetainAggregationAndFencePodUID(t *testing.T) {
	image := testImage()

	workload, err := buildDeployment("demo", "flexpool", image)
	if err != nil {
		t.Fatal(err)
	}

	workload.UID = "workload"
	start := time.Now().UTC().Truncate(time.Second)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-pod", Namespace: "demo", UID: "current-pod", Labels: map[string]string{deploymentLabel: image.ID}, OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: workload.Name, UID: workload.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "worker"}}
	owned := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "demo", CreationTimestamp: metav1.NewTime(start)}, InvolvedObject: corev1.ObjectReference{UID: pod.UID, Name: pod.Name}, Reason: "Pulling", FirstTimestamp: metav1.NewTime(start.Add(time.Second)), LastTimestamp: metav1.NewTime(start.Add(2 * time.Second)), Count: 2, Series: &corev1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(start.Add(4 * time.Second))}}
	foreign := owned.DeepCopy()
	foreign.Name = "foreign"
	foreign.InvolvedObject.UID = "old-pod"
	server := testServer(fake.NewSimpleClientset(workload, pod, owned, foreign), image)
	response := apiRequest(server, http.MethodGet, "/api/deployments/"+image.ID+"/events?node=worker", "")

	var events []struct {
		PodUID  string    `json:"podUID"`
		FirstAt time.Time `json:"firstAt"`
		At      time.Time `json:"at"`
		Count   int32     `json:"count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || len(events) != 1 || events[0].PodUID != string(pod.UID) || events[0].Count != 3 ||
		!events[0].FirstAt.Equal(start.Add(time.Second)) || !events[0].At.Equal(start.Add(4*time.Second)) {
		t.Fatalf("event timing or Pod fencing was lost: %d %s", response.Code, response.Body)
	}
}
