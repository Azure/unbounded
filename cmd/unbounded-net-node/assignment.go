// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

const assignmentRetryInterval = 2 * time.Second

var errAssignmentChanged = errors.New("assignment changed after adoption")

type nodeAssignment struct {
	site        string
	gatewayPool string
	nodeUID     types.UID
}

func assignmentFromNode(node *corev1.Node) (nodeAssignment, error) {
	if node == nil {
		return nodeAssignment{}, fmt.Errorf("node is nil")
	}

	site := ""
	if node.Labels != nil {
		site = strings.TrimSpace(node.Labels[unboundedv1alpha3.MachineSiteLabelKey])
		if site == "" {
			site = strings.TrimSpace(node.Labels[unboundednetv1alpha1.SiteLabelKey])
		}
	}

	gatewayPool := ""
	if node.Labels != nil {
		gatewayPool = node.Labels[unboundednetv1alpha1.GatewayPoolLabelKey]
	}

	if gatewayPool != "" {
		if problems := validation.IsValidLabelValue(gatewayPool); len(problems) != 0 {
			return nodeAssignment{}, fmt.Errorf("node %q has invalid %s label value %q: %s", node.Name, unboundednetv1alpha1.GatewayPoolLabelKey, gatewayPool, strings.Join(problems, "; "))
		}
	}

	return nodeAssignment{site: site, gatewayPool: gatewayPool, nodeUID: node.UID}, nil
}

func validateAdoptedNodeAssignment(node *corev1.Node, adopted nodeAssignment) error {
	current, err := assignmentFromNode(node)
	if err != nil {
		return err
	}

	if current.nodeUID != adopted.nodeUID || current.site != adopted.site || current.gatewayPool != adopted.gatewayPool {
		return fmt.Errorf("%w: Node %q adopted site=%q gatewayPool=%q UID=%q, current site=%q gatewayPool=%q UID=%q", errAssignmentChanged, node.Name, adopted.site, adopted.gatewayPool, adopted.nodeUID, current.site, current.gatewayPool, current.nodeUID)
	}

	return nil
}

func waitForNodeAssignment(ctx context.Context, clientset kubernetes.Interface, nodeName string) (nodeAssignment, error) {
	for {
		node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return nodeAssignment{}, ctx.Err()
			}

			klog.Warningf("Failed to get Node %s while waiting for Site assignment: %v", nodeName, err)

			if err := waitForAssignmentRetry(ctx); err != nil {
				return nodeAssignment{}, err
			}

			continue
		}

		assignment, err := assignmentFromNode(node)
		if err != nil {
			return nodeAssignment{}, err
		}

		if assignment.site != "" {
			return assignment, nil
		}

		klog.Infof("Node %s has no Site assignment yet, watching for %s", nodeName, unboundedv1alpha3.MachineSiteLabelKey)

		watcher, err := clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", nodeName).String(),
			ResourceVersion: node.ResourceVersion,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nodeAssignment{}, ctx.Err()
			}

			klog.Warningf("Failed to watch Node %s for Site assignment: %v", nodeName, err)

			if err := waitForAssignmentRetry(ctx); err != nil {
				return nodeAssignment{}, err
			}

			continue
		}

		restartWatch := false
		for !restartWatch {
			select {
			case <-ctx.Done():
				watcher.Stop()

				return nodeAssignment{}, ctx.Err()
			case event, ok := <-watcher.ResultChan():
				if !ok {
					klog.Warningf("Node %s assignment watch closed; restarting from a fresh GET", nodeName)

					restartWatch = true

					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					updated, ok := event.Object.(*corev1.Node)
					if !ok || updated.Name != nodeName {
						continue
					}

					assignment, err := assignmentFromNode(updated)
					if err != nil {
						watcher.Stop()

						return nodeAssignment{}, err
					}

					if assignment.site != "" {
						watcher.Stop()

						return assignment, nil
					}
				case watch.Deleted:
					klog.Warningf("Node %s was deleted while waiting for Site assignment; restarting from a fresh GET", nodeName)

					restartWatch = true
				case watch.Error:
					klog.Warningf("Node %s assignment watch error: %v", nodeName, apierrors.FromObject(event.Object))

					restartWatch = true
				case watch.Bookmark:
					continue
				}
			}
		}

		watcher.Stop()
	}
}

func waitForAssignmentRetry(ctx context.Context) error {
	timer := time.NewTimer(assignmentRetryInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func acknowledgeEffectiveAssignment(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, podName string,
	podUID types.UID,
	nodeName string,
	adopted nodeAssignment,
) error {
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Node %q before acknowledging assignment: %w", nodeName, err)
	}

	if err := validateAdoptedNodeAssignment(node, adopted); err != nil {
		return err
	}

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get own Pod %s/%s before acknowledging assignment: %w", namespace, podName, err)
	}

	if pod.UID != podUID {
		return fmt.Errorf("own Pod %s/%s UID changed from %q to %q", namespace, podName, podUID, pod.UID)
	}

	if pod.Spec.NodeName != nodeName {
		return fmt.Errorf("own Pod %s/%s is bound to Node %q, expected %q", namespace, podName, pod.Spec.NodeName, nodeName)
	}

	effectiveSite, siteAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveSiteAnnotation]

	effectiveGatewayPool, gatewayAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation]
	if siteAcknowledged && gatewayAcknowledged &&
		effectiveSite == adopted.site &&
		effectiveGatewayPool == adopted.gatewayPool {
		return nil
	}

	operations := []map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(podUID)},
	}
	if pod.Annotations == nil {
		operations = append(operations, map[string]interface{}{
			"op":   "add",
			"path": "/metadata/annotations",
			"value": map[string]string{
				unboundednetv1alpha1.EffectiveSiteAnnotation:        adopted.site,
				unboundednetv1alpha1.EffectiveGatewayPoolAnnotation: adopted.gatewayPool,
			},
		})
	} else {
		operations = append(operations,
			map[string]interface{}{"op": "add", "path": "/metadata/annotations/" + escapeJSONPointer(unboundednetv1alpha1.EffectiveSiteAnnotation), "value": adopted.site},
			map[string]interface{}{"op": "add", "path": "/metadata/annotations/" + escapeJSONPointer(unboundednetv1alpha1.EffectiveGatewayPoolAnnotation), "value": adopted.gatewayPool},
		)
	}

	patch, err := json.Marshal(operations)
	if err != nil {
		return fmt.Errorf("marshal own Pod assignment acknowledgment: %w", err)
	}

	if _, err := clientset.CoreV1().Pods(namespace).Patch(ctx, podName, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch own Pod %s/%s assignment acknowledgment: %w", namespace, podName, err)
	}

	return nil
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
