// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestOperatorStreamingUsesNodeNetwork(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		objects, err := decodeManifests(testEnv(t), applyMutator("gantry:test", "gantry-node-config:test", "hash",
			artifactStreamingConfig{Enabled: enabled, NodeSelector: map[string]string{"pool": "streaming"}}, nil))
		if err != nil {
			t.Fatal(err)
		}

		found := false

		for _, object := range objects {
			if object.GetKind() != "DaemonSet" || object.GetName() != daemonSetName {
				continue
			}

			found = true

			var ds appsv1.DaemonSet
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &ds); err != nil {
				t.Fatal(err)
			}

			pod := ds.Spec.Template.Spec
			if pod.HostNetwork != enabled {
				t.Fatalf("streaming=%v hostNetwork=%v", enabled, pod.HostNetwork)
			}

			wantPolicy := corev1.DNSClusterFirst
			if enabled {
				wantPolicy = corev1.DNSClusterFirstWithHostNet
			}

			if pod.DNSPolicy != wantPolicy {
				t.Fatalf("streaming=%v DNS=%s, want %s", enabled, pod.DNSPolicy, wantPolicy)
			}

			var mirrorFound bool

			for _, container := range pod.Containers {
				if container.Name != agentContainerName {
					continue
				}

				for _, port := range container.Ports {
					if port.Name != "mirror" {
						continue
					}

					mirrorFound = true

					if enabled && (port.HostPort != 0 || port.HostIP != "") {
						t.Fatalf("host-networked mirror retains port mapping: %+v", port)
					}

					if !enabled && (port.HostPort != 5000 || port.HostIP != "127.0.0.1") {
						t.Fatalf("normal mirror lost port mapping: %+v", port)
					}
				}

				env := map[string]string{}
				for _, entry := range container.Env {
					env[entry.Name] = entry.Value
				}

				for name, want := range map[string]string{artifactStreamingEnabledEnv: "true", "GANTRY_MIRROR_LISTEN": "127.0.0.1:5000", "GANTRY_MIRROR_BIND_ALLOW_NON_LOOPBACK": "false"} {
					if enabled && env[name] != want {
						t.Fatalf("%s=%q, want %q", name, env[name], want)
					}

					if !enabled {
						if _, exists := env[name]; exists {
							t.Fatalf("disabled mode retains %s", name)
						}
					}
				}
			}

			if !mirrorFound {
				t.Fatal("mirror port not found")
			}

			before := object.DeepCopy()
			if enabled {
				if err := applyArtifactStreamingMode(object); err != nil {
					t.Fatal(err)
				}

				if !reflect.DeepEqual(before.Object, object.Object) {
					t.Fatal("streaming network mutation is not idempotent")
				}
			}
		}

		if !found {
			t.Fatal("agent DaemonSet missing")
		}
	}
}
