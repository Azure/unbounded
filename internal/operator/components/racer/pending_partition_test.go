// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racercore "github.com/Azure/unbounded/internal/racer"
)

func TestMigrationPendingPartition(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, shape := range []string{"controller", "same-or", "destination", "template", "missing", "no-terms", "empty", "multiple", "other-or", "unbounded-or", "extra-field", "wrong-key", "wrong-op", "stale", "foreign", "unknown", "unknown-uid"} {
			t.Run(map[bool]string{false: "host-source/", true: "pod-source/"}[reverse]+shape, func(t *testing.T) {
				env := testEnv(t)
				cfg := occupancyConfig(env.Namespace)
				cfg.HostNetwork = true
				cfg.PodNetworkNodes = []string{"pod-node"}
				sets, err := racercore.DesiredDaemonSets(cfg)
				require.NoError(t, err)

				for _, ds := range sets {
					require.NoError(t, env.Client.Create(t.Context(), ds))
				}

				source, destination := sets[0], sets[1]
				target, destinationNode := "host-node", "pod-node"

				if reverse {
					source, destination = destination, source
					target, destinationNode = destinationNode, target
				}

				baseline, _, err := migrationPlan(t.Context(), env, cfg)
				require.NoError(t, err)

				for _, ds := range baseline {
					if ds.Name == destination.Name {
						destination = ds
					}
				}

				if shape == "destination" {
					target = destinationNode
				}
				// The DS controller copies the template, then replaces required node
				// affinity with its chosen node's exact field selector before binding.
				pod := &corev1.Pod{ObjectMeta: *source.Spec.Template.ObjectMeta.DeepCopy(), Spec: *source.Spec.Template.Spec.DeepCopy()}
				pod.Name, pod.Namespace = "pending-source", env.Namespace
				pod.Finalizers = []string{"test/drain"}
				pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(source, source.GroupVersionKind())}
				pod.OwnerReferences[0].APIVersion, pod.OwnerReferences[0].Kind = "apps/v1", "DaemonSet"
				term := corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{target}}}}

				selector := &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{term}}
				if shape != "template" {
					pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = selector
				}

				switch shape {
				case "same-or":
					selector.NodeSelectorTerms = append(selector.NodeSelectorTerms, *term.DeepCopy())
				case "missing":
					pod.Spec.Affinity = nil
				case "no-terms":
					selector.NodeSelectorTerms = nil
				case "extra-field":
					selector.NodeSelectorTerms[0].MatchFields = append(selector.NodeSelectorTerms[0].MatchFields, term.MatchFields[0])
				case "empty":
					selector.NodeSelectorTerms[0].MatchFields[0].Values = []string{""}
				case "multiple":
					selector.NodeSelectorTerms[0].MatchFields[0].Values = []string{target, destinationNode}
				case "other-or":
					other := term.DeepCopy()
					other.MatchFields[0].Values = []string{destinationNode}
					selector.NodeSelectorTerms = append(selector.NodeSelectorTerms, *other)
				case "unbounded-or":
					selector.NodeSelectorTerms = append(selector.NodeSelectorTerms, corev1.NodeSelectorTerm{})
				case "wrong-key":
					selector.NodeSelectorTerms[0].MatchFields[0].Key = "kubernetes.io/hostname"
				case "wrong-op":
					selector.NodeSelectorTerms[0].MatchFields[0].Operator = corev1.NodeSelectorOpNotIn
				case "stale":
					pod.OwnerReferences[0].UID = "stale"
				case "foreign":
					pod.OwnerReferences[0].Kind = "Deployment"
				case "unknown":
					pod.OwnerReferences = nil
				case "unknown-uid":
					source.UID = ""
					require.NoError(t, env.Client.Update(t.Context(), source))
				}

				require.NoError(t, env.Client.Create(t.Context(), pod))

				valid := shape == "controller" || shape == "same-or"

				for _, state := range []string{"pending", "bound", "terminating", "deleted"} {
					if state == "bound" {
						pod.Spec.NodeName = target
						require.NoError(t, env.Client.Update(t.Context(), pod))
					}

					if state == "terminating" {
						require.NoError(t, env.Client.Delete(t.Context(), pod))
					}

					if state == "deleted" {
						require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
						pod.Finalizers = nil
						require.NoError(t, env.Client.Update(t.Context(), pod))
					}

					got, _, err := migrationPlan(t.Context(), env, cfg)
					require.NoError(t, err)

					for _, ds := range got {
						if ds.Name != destination.Name {
							continue
						}

						if state == "deleted" || (shape != "destination" && (valid || state != "pending")) {
							require.True(t, reflect.DeepEqual(destination.Spec.Template, ds.Spec.Template), "whole destination template changed: %s", state)
						} else {
							require.False(t, permitsNode(ds, destinationNode), state)
						}
					}
				}
			})
		}
	}
}
