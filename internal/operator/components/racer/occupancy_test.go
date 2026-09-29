// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	racercore "github.com/Azure/unbounded/internal/racer"
)

func gantrySocketPod(t *testing.T, env *component.Env, socketRoot string) *corev1.Pod {
	t.Helper()

	objects, err := env.DecodeManifestFiles(gantrymanifests.Manifests, []string{"daemonset.yaml"}, nil)
	require.NoError(t, err)
	require.Len(t, objects, 1)

	plan := component.NewPlan()
	plan.Add(component.Operation{Kind: component.OpApply, Object: objects[0], Component: "gantry", Overridable: true})

	raw, err := os.ReadFile("../../../../deploy/gantry/examples/racer-operator-overrides.yaml")
	require.NoError(t, err)

	var cm corev1.ConfigMap
	require.NoError(t, yaml.Unmarshal(raw, &cm))
	entries, problems, err := override.Parse(cm.Data)
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, override.ValidateErr(entries))
	require.False(t, override.Apply(plan, entries, nil).Failed())

	var ds appsv1.DaemonSet
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(plan.Operations[0].Object.Object, &ds))
	pod := &corev1.Pod{ObjectMeta: *ds.Spec.Template.ObjectMeta.DeepCopy(), Spec: *ds.Spec.Template.Spec.DeepCopy()}
	pod.Name, pod.Namespace = "gantry-client", env.Namespace
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "gantry", UID: "gantry-uid", Controller: ptr.To(true)}}
	// Keep the real integration template, varying only its shared socket scope.
	found := false

	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == "racer-sockets" {
			require.Equal(t, "/run/racer/gantry", v.HostPath.Path)
			v.HostPath.Path = socketRoot
			found = true
		}
	}

	require.True(t, found)

	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].VolumeMounts {
			mount := &pod.Spec.Containers[i].VolumeMounts[j]
			if mount.Name == "racer-sockets" {
				mount.MountPath = socketRoot
			}
		}
	}

	return pod
}

func occupancyConfig(namespace string) racercore.WorkloadConfig {
	return racercore.WorkloadConfig{
		Cluster: "00000000-0000-0000-0000-000000000001", Namespace: namespace,
		ControlURL: "https://racer-controller:8443", BootstrapTrustConfigMap: "racer-bootstrap-trust",
		DataplaneImage: "racer-dataplane:test", PeerPort: 8082,
		DataplaneServiceAccount: dataplaneName, DaemonSetName: dataplaneName,
	}
}

func TestMigrationSocketOnlyPodNames(t *testing.T) {
	for _, name := range []string{"gantry", "loadgen", "diagnostic", "arbitrary"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/name": name}}, Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "sockets", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/racer"}}}},
		}}
		require.False(t, migrationPod(pod), "socket sharing does not require a trusted client name")
	}

	require.False(t, migrationPod(&corev1.ConfigMap{}))
}

func TestMigrationGantrySocketClients(t *testing.T) {
	for _, mode := range []string{"default-single", "host-empty-exceptions", "mixed"} {
		for _, socketRoot := range []string{"/run/racer/gantry", "/run/racer"} {
			t.Run(mode+socketRoot, func(t *testing.T) {
				env := testEnv(t)
				cfg := occupancyConfig(env.Namespace)

				cfg.HostNetwork = mode != "default-single"
				if mode == "mixed" {
					cfg.PodNetworkNodes = []string{"node-a"}
				}

				want, err := racercore.DesiredDaemonSets(cfg)
				require.NoError(t, err)

				for _, ds := range want {
					require.NoError(t, env.Client.Create(t.Context(), ds.DeepCopy()))
				}

				for i, node := range []string{"node-a", "node-b", ""} {
					pod := gantrySocketPod(t, env, socketRoot)
					pod.Name += string(rune('a' + i))
					pod.Spec.NodeName = node
					require.NoError(t, env.Client.Create(t.Context(), pod))
				}

				got, result, err := migrationPlan(t.Context(), env, cfg)
				require.NoError(t, err)
				require.True(t, result.Ready, "%+v", result)
				require.Equal(t, want, got, "socket clients must not alter placement, even while unscheduled")
			})
		}
	}
}

func TestMigrationForeignDataplaneClaims(t *testing.T) {
	for _, claim := range []string{"identity", "slabs", "host-label", "pod-label", "host-owner", "pod-owner", "wrong-kind", "wrong-api", "current-owner"} {
		for _, node := range []string{"node-a", ""} {
			t.Run(claim+"/"+node, func(t *testing.T) {
				env := testEnv(t)
				cfg := occupancyConfig(env.Namespace)
				sets, err := racercore.DesiredDaemonSets(cfg)
				require.NoError(t, err)

				ds := sets[0]
				require.NoError(t, env.Client.Create(t.Context(), ds))
				pod := gantrySocketPod(t, env, "/run/racer")
				pod.Spec.NodeName = node

				switch claim {
				case "identity", "slabs":
					pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: claim, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/racer/" + claim}}})
				case "host-label":
					pod.Labels["app.kubernetes.io/name"] = dataplaneName
				case "pod-label":
					pod.Labels["app.kubernetes.io/name"] = racercore.PodNetworkDaemonSetName
				default:
					owner := &pod.OwnerReferences[0]
					owner.Name, owner.UID = dataplaneName, types.UID("stale")

					switch claim {
					case "pod-owner":
						owner.Name = racercore.PodNetworkDaemonSetName
					case "wrong-kind":
						owner.Kind, owner.UID = "Deployment", ds.UID
					case "wrong-api":
						owner.APIVersion, owner.UID = "other/v1", ds.UID
					case "current-owner":
						owner.UID = ds.UID
					}
				}

				pod.Finalizers = []string{"test/drain"}
				require.NoError(t, env.Client.Create(t.Context(), pod))

				for _, terminating := range []bool{false, true} {
					if terminating {
						require.NoError(t, env.Client.Delete(t.Context(), pod))
					}

					got, result, err := migrationPlan(t.Context(), env, cfg)
					require.NoError(t, err)

					if claim == "current-owner" {
						require.True(t, result.Ready)
						require.True(t, permitsNode(got[0], "node-a"))
					} else {
						require.Equal(t, "MigrationBlocked", result.Reason)
						require.False(t, permitsNode(got[0], "node-a"), "Gantry name must not exempt a dataplane claim; terminating=%t", terminating)
						require.Equal(t, node != "", permitsNode(got[0], "node-b"))
					}
				}
			})
		}
	}
}
