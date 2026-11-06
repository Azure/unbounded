// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type streamingNodeRunner struct {
	recordingRunner
	outputs [][]byte
	failAt  int
}

func (r *streamingNodeRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	_, _ = r.recordingRunner.Run(ctx, stdin, name, args...)
	if len(r.commands) == r.failAt {
		return nil, errors.New("kubectl failed")
	}

	if len(r.outputs) == 0 {
		return nil, errors.New("unexpected command")
	}

	output := r.outputs[0]
	r.outputs = r.outputs[1:]

	return output, nil
}

func readyConfiguratorPod(node string) corev1.Pod {
	controller := true

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "configure-" + node,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "gantry-overlaybd-config", UID: "current-ds", Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func TestStreamingConfiguratorRequiresExactReadyNodeCoverage(t *testing.T) {
	for _, test := range []struct {
		name      string
		modify    func(*appsv1.DaemonSet, *corev1.PodList)
		failAt    int
		wantError string
	}{
		{name: "matching nodes"},
		{name: "equally sized wrong pool", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) {
			pods.Items = []corev1.Pod{readyConfiguratorPod("other-a"), readyConfiguratorPod("other-b")}
		}, wantError: "missing=[node-a node-b] unexpected=[other-a other-b]"},
		{name: "one wrong node", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) { pods.Items[1].Spec.NodeName = "other" }, wantError: "missing=[node-b] unexpected=[other]"},
		{name: "not ready", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) {
			pods.Items[1].Status.Conditions[0].Status = corev1.ConditionFalse
		}, wantError: "missing=[node-b]"},
		{name: "terminating", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) {
			now := metav1.Now()
			pods.Items[1].DeletionTimestamp = &now
		}, wantError: "missing=[node-b]"},
		{name: "completed", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) { pods.Items[1].Status.Phase = corev1.PodSucceeded }, wantError: "missing=[node-b]"},
		{name: "old owner", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) {
			pods.Items[1].OwnerReferences[0].UID = types.UID("old-ds")
		}, wantError: "missing=[node-b]"},
		{name: "unowned pod", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) { pods.Items[1].OwnerReferences = nil }, wantError: "missing=[node-b]"},
		{name: "duplicate", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) { pods.Items[1].Spec.NodeName = "node-a" }, wantError: "duplicate"},
		{name: "unscheduled", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) { pods.Items[1].Spec.NodeName = "" }, wantError: "unscheduled"},
		{name: "unrelated pods ignored", modify: func(_ *appsv1.DaemonSet, pods *corev1.PodList) {
			extra := readyConfiguratorPod("other")
			extra.OwnerReferences[0].UID = "other-ds"
			pods.Items = append(pods.Items, extra)
		}},
		{name: "count mismatch", modify: func(ds *appsv1.DaemonSet, _ *corev1.PodList) { ds.Status.NumberReady = 1 }, wantError: "ready on 1/2"},
		{name: "missing selector", modify: func(ds *appsv1.DaemonSet, _ *corev1.PodList) { ds.Spec.Selector = nil }, wantError: "requires target nodes"},
		{name: "empty selector", modify: func(ds *appsv1.DaemonSet, _ *corev1.PodList) { ds.Spec.Selector = &metav1.LabelSelector{} }, wantError: "empty pod selector"},
		{name: "get daemonset failure", failAt: 1, wantError: "kubectl failed"},
		{name: "list pods failure", failAt: 2, wantError: "kubectl failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ds := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{UID: "current-ds"}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "configurator"}}}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2}}
			pods := corev1.PodList{Items: []corev1.Pod{readyConfiguratorPod("node-b"), readyConfiguratorPod("node-a")}}
			// Use a stable ordering for mutations while retaining set-based validation.
			slices.Reverse(pods.Items)

			if test.modify != nil {
				test.modify(&ds, &pods)
			}

			encode := func(value any) []byte {
				t.Helper()

				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}

				return data
			}
			runner := &streamingNodeRunner{outputs: [][]byte{encode(ds), encode(pods)}, failAt: test.failAt}
			bench := &benchmark{commands: runner}

			err := bench.validateStreamingDaemonSetNodes(t.Context(), "gantry-system", "gantry-overlaybd-config", []string{"node-b", "node-a"})
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}

			if len(runner.commands) >= 2 && !slices.Equal(runner.commands[1].args, []string{"-n", "gantry-system", "get", "pods", "-l", "app=configurator", "-o", "json"}) {
				t.Fatalf("pod lookup = %v", runner.commands[1].args)
			}
		})
	}
}
