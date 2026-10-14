// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestNodeAssignmentControllerRestartDecisions(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		terminating bool
		target      bool
		desiredSite string
		desiredPool string
		explicit    bool
		wantDeleted bool
	}{
		{
			name: "site mismatch",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-b",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
			},
			target:      true,
			wantDeleted: true,
		},
		{
			name: "gateway promotion",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
			},
			target:      true,
			wantDeleted: true,
		},
		{
			name: "gateway changed",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "pool-b",
			},
			target:      true,
			wantDeleted: true,
		},
		{
			name: "gateway demotion",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "pool-a",
			},
			target:      true,
			desiredSite: "site-a",
			explicit:    true,
			wantDeleted: true,
		},
		{
			name: "site unset",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
			},
			target:      true,
			explicit:    true,
			wantDeleted: true,
		},
		{
			name: "unchanged",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "pool-a",
			},
			target: true,
		},
		{name: "pending both", target: true},
		{
			name: "pending gateway",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation: "site-a",
			},
			target: true,
		},
		{
			name: "terminating mismatch",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-b",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
			},
			target:      true,
			terminating: true,
		},
		{
			name: "unrelated pod",
			annotations: map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-b",
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desiredSite := "site-a"
			desiredPool := "pool-a"

			if tt.explicit {
				desiredSite = tt.desiredSite
				desiredPool = tt.desiredPool
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{},
			}}
			if desiredSite != "" {
				node.Labels[unboundedv1alpha3.MachineSiteLabelKey] = desiredSite
			}

			if desiredPool != "" {
				node.Labels[unboundednetv1alpha1.GatewayPoolLabelKey] = desiredPool
			}

			pod := assignmentTestPod(tt.target, tt.annotations)
			if tt.terminating {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			}

			client := kubefake.NewClientset(pod.DeepCopy(), node.DeepCopy())
			controller := assignmentTestController(t, client, node, pod)

			if err := controller.syncNode(t.Context(), node.Name); err != nil {
				t.Fatal(err)
			}

			_, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
			if tt.wantDeleted && err == nil {
				t.Fatal("expected Pod deletion")
			}

			if !tt.wantDeleted && err != nil {
				t.Fatalf("Pod unexpectedly deleted: %v", err)
			}

			if tt.wantDeleted {
				options := podDeleteOptions(t, client)
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != pod.UID ||
					options.Preconditions.ResourceVersion == nil {
					t.Fatalf("delete preconditions = %#v, want UID %s and a resourceVersion", options.Preconditions, pod.UID)
				}
			}
		})
	}
}

func TestNodeAssignmentControllerSkipsOldTerminatingPodAndPendingReplacement(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-b"},
	}}
	oldPod := assignmentTestPod(true, map[string]string{
		unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
		unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
	})
	now := metav1.Now()
	oldPod.DeletionTimestamp = &now
	newPod := assignmentTestPod(true, nil)
	newPod.Name = "agent-new"
	newPod.UID = "pod-new"

	client := kubefake.NewClientset(oldPod.DeepCopy(), newPod.DeepCopy())

	controller := assignmentTestController(t, client, node, oldPod, newPod)
	if err := controller.syncNode(t.Context(), node.Name); err != nil {
		t.Fatal(err)
	}

	for _, pod := range []*corev1.Pod{oldPod, newPod} {
		if _, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{}); err != nil {
			t.Fatalf("Pod %s unexpectedly deleted: %v", pod.Name, err)
		}
	}
}

func podDeleteOptions(t *testing.T, client *kubefake.Clientset) metav1.DeleteOptions {
	t.Helper()

	for _, action := range client.Actions() {
		if action.GetVerb() != "delete" {
			continue
		}

		deleteAction, ok := action.(interface{ GetDeleteOptions() metav1.DeleteOptions })
		if !ok {
			t.Fatalf("delete action type = %T", action)
		}

		return deleteAction.GetDeleteOptions()
	}

	t.Fatal("no delete action recorded")

	return metav1.DeleteOptions{}
}

func TestNodeAssignmentControllerRevalidatesStaleNodeCache(t *testing.T) {
	ack := map[string]string{
		unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-b",
		unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
	}
	cachedNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-a"},
	}}
	liveNode := cachedNode.DeepCopy()
	liveNode.Labels[unboundedv1alpha3.MachineSiteLabelKey] = "site-b"
	pod := assignmentTestPod(true, ack)

	client := kubefake.NewClientset(pod.DeepCopy(), liveNode)

	controller := assignmentTestController(t, client, cachedNode, pod)
	if err := controller.syncNode(t.Context(), cachedNode.Name); err != nil {
		t.Fatal(err)
	}

	if _, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Pod acknowledging the live assignment was deleted: %v", err)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatalf("unexpected delete action: %#v", action)
		}
	}
}

func TestNodeAssignmentControllerRevalidatesStalePodCache(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-b"},
	}}
	cachedPod := assignmentTestPod(true, map[string]string{
		unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
		unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
	})
	livePod := cachedPod.DeepCopy()
	livePod.Annotations = map[string]string{
		unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-b",
		unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
	}

	client := kubefake.NewClientset(livePod, node.DeepCopy())

	controller := assignmentTestController(t, client, node, cachedPod)
	if err := controller.syncNode(t.Context(), node.Name); err != nil {
		t.Fatal(err)
	}

	if _, err := client.CoreV1().Pods(livePod.Namespace).Get(t.Context(), livePod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Pod with a current live acknowledgment was deleted: %v", err)
	}
}

func TestNodeAssignmentControllerSkipsDeletedLiveNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "site-b"},
	}}
	pod := assignmentTestPod(true, map[string]string{
		unboundednetv1alpha1.EffectiveSiteAnnotation:        "site-a",
		unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: "",
	})

	client := kubefake.NewClientset(pod.DeepCopy())

	controller := assignmentTestController(t, client, node, pod)
	if err := controller.syncNode(t.Context(), node.Name); err != nil {
		t.Fatal(err)
	}

	if _, err := client.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Pod deleted after its Node was deleted: %v", err)
	}
}

func assignmentTestPod(target bool, annotations map[string]string) *corev1.Pod {
	controller := true

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "system",
			Name:        "agent",
			UID:         types.UID("pod-uid"),
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
	}
	if target {
		pod.Labels = map[string]string{
			"app.kubernetes.io/name":      nodeAgentDaemonSetName,
			"app.kubernetes.io/component": "node",
		}
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "DaemonSet",
			Name:       nodeAgentDaemonSetName,
			UID:        "ds-uid",
			Controller: &controller,
		}}
	}

	return pod
}

func assignmentTestController(t *testing.T, client *kubefake.Clientset, node *corev1.Node, pods ...*corev1.Pod) *NodeAssignmentController {
	t.Helper()

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatal(err)
	}

	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		NodeAgentPodNodeNameIndex: NodeAgentPodNodeNameIndexFunc,
	})
	for _, pod := range pods {
		if err := podIndexer.Add(pod); err != nil {
			t.Fatal(err)
		}
	}

	return &NodeAssignmentController{
		clientset:  client,
		nodeLister: corev1listers.NewNodeLister(nodeIndexer),
		podIndexer: podIndexer,
		queue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
}
