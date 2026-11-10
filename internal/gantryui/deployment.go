// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"encoding/json"
	"errors"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	managerLabel    = "app.kubernetes.io/managed-by"
	deploymentLabel = "gantry.unbounded-cloud.io/deployment"
	imageAnnotation = "gantry.unbounded-cloud.io/demo-image"
)

type Deployment struct {
	ID      string    `json:"id"`
	UID     string    `json:"uid"`
	Name    string    `json:"name"`
	Image   Image     `json:"image"`
	Created time.Time `json:"created"`
}

func deploymentName(id string) string { return "gantry-demo-" + id }

func buildDeployment(namespace, pool string, image Image) (*appsv1.DaemonSet, error) {
	encoded, err := json.Marshal(image)
	if err != nil {
		return nil, err
	}

	if len(encoded) > 200<<10 {
		return nil, errors.New("image metadata exceeds deployment annotation limit")
	}

	labels := map[string]string{managerLabel: "gantry-ui", deploymentLabel: image.ID}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploymentName(image.ID), Namespace: namespace, Labels: labels,
			Annotations: map[string]string{imageAnnotation: string(encoded)},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.azure.com/agentpool":     pool,
						"kubernetes.azure.com/nodepool-type": "FlexNodes",
						"kubernetes.io/os":                   "linux",
						"kubernetes.io/arch":                 image.Architecture,
					},
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: "image", Image: image.Reference, ImagePullPolicy: corev1.PullAlways,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
					}},
				},
			},
		},
	}, nil
}

func describeDeployment(ds *appsv1.DaemonSet) (Deployment, error) {
	if ds.Labels[managerLabel] != "gantry-ui" {
		return Deployment{}, errors.New("DaemonSet is not owned by the Gantry UI")
	}

	var image Image
	if err := json.Unmarshal([]byte(ds.Annotations[imageAnnotation]), &image); err != nil {
		return Deployment{}, errors.New("DaemonSet image metadata is missing or invalid")
	}

	if ds.Name != deploymentName(image.ID) || ds.Labels[deploymentLabel] != image.ID ||
		len(ds.Spec.Template.Spec.Containers) != 1 || ds.Spec.Template.Spec.Containers[0].Image != image.Reference {
		return Deployment{}, errors.New("DaemonSet no longer matches its recorded demo image")
	}

	return Deployment{ID: image.ID, UID: string(ds.UID), Name: ds.Name, Image: image, Created: ds.CreationTimestamp.Time}, nil
}

func podLifecycle(pod *corev1.Pod) (phase, reason string) {
	if pod == nil {
		return "queued", "Waiting for a DaemonSet Pod"
	}

	if pod.DeletionTimestamp != nil {
		return "terminating", "Pod is terminating"
	}

	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if waiting := status.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return "failed", waiting.Reason + ": " + waiting.Message
			}
		}

		if terminated := status.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
			return "failed", terminated.Reason + ": " + terminated.Message
		}
	}

	if pod.Status.Phase == corev1.PodFailed {
		return "failed", pod.Status.Reason + ": " + pod.Status.Message
	}

	if pod.Status.Phase == corev1.PodSucceeded {
		return "completed", "Container completed successfully"
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return "ready", "Pod Ready"
		}

		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return "scheduling", condition.Message
		}
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running != nil {
			return "running", "Container running; Pod not yet Ready"
		}
	}

	if pod.Spec.NodeName == "" {
		return "scheduling", "Waiting for scheduling"
	}

	return "preparing", "Preparing image, filesystem, or container; Kubernetes does not report unpack progress"
}
