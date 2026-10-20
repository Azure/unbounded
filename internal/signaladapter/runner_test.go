// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestCleanupDeletesCreatedBackends(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "run-p0",
			"namespace": "project-signal",
			"uid":       "pod-uid",
		},
	}}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]any{
			"name":      "run-p1",
			"namespace": "project-signal",
			"uid":       "job-uid",
		},
	}}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			podGVR:    "PodList",
			mpiJobGVR: "MPIJobList",
		},
		pod,
		job,
	)
	runner := &Runner{Dynamic: client, PollInterval: time.Millisecond}

	err := runner.cleanup(context.Background(), "project-signal", []createdResource{
		{gvr: podGVR, resource: pod, participantIndex: 0},
		{gvr: mpiJobGVR, resource: job, participantIndex: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{
		{gvr: podGVR, name: pod.GetName()},
		{gvr: mpiJobGVR, name: job.GetName()},
	} {
		_, err := client.Resource(item.gvr).Namespace("project-signal").
			Get(context.Background(), item.name, metav1.GetOptions{})
		if err == nil {
			t.Fatalf("%s still exists", item.name)
		}
	}
}
