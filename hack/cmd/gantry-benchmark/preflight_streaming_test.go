// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestArtifactStreamingPreflightChecksBothConfiguratorNodeSets(t *testing.T) {
	for _, wrongPool := range []string{"", "gantry-containerd-config", "gantry-overlaybd-config"} {
		t.Run("wrong pool="+wrongPool, func(t *testing.T) {
			encode := func(value any) []byte {
				t.Helper()

				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}

				return data
			}
			runner := &streamingNodeRunner{}

			for _, name := range []string{"gantry-containerd-config", "gantry-overlaybd-config"} {
				ds := appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{UID: "current-ds"},
					Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
					Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
				}
				pod := readyConfiguratorPod("target")

				pod.OwnerReferences[0].Name = name
				if name == wrongPool {
					pod.Spec.NodeName = "wrong-pool"
				}

				runner.outputs = append(runner.outputs, encode(ds), encode(corev1.PodList{Items: []corev1.Pod{pod}}))

				if name == wrongPool {
					break
				}
			}

			runner.outputs = append(runner.outputs, encode(map[string]any{"data": map[string]string{"config.yaml": "artifact_streaming_enabled: true\nupstream_registries:\n  - name: app.azurecr.io\n    endpoint: http://127.0.0.1:8578?ns=app.azurecr.io\n"}}))
			bench := &benchmark{commands: runner, config: benchmarkConfig{GantryNamespace: "gantry-system", GantryConfigMap: "gantry-config"}}

			err := bench.checkArtifactStreaming(t.Context(), benchmarkState{GantryACRLoginServer: "app.azurecr.io"}, []string{"target"})
			if wrongPool == "" {
				if err != nil {
					t.Fatal(err)
				}

				if len(runner.commands) != 5 {
					t.Fatalf("commands = %d, want both node-set checks then config validation", len(runner.commands))
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), wrongPool) || !strings.Contains(err.Error(), "missing=[target] unexpected=[wrong-pool]") {
					t.Fatalf("wrong pool passed: %v", err)
				}

				for _, command := range runner.commands {
					if strings.Contains(strings.Join(command.args, " "), "configmap") {
						t.Fatal("preflight continued after node coverage failed")
					}
				}
			}
		})
	}
}
