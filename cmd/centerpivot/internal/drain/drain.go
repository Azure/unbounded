// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package drain moves pods off a node with kubectl drain's rules
// (k8s.io/kubectl/pkg/drain), one step at a time so a caller never blocks on
// it, and tracks the workloads whose pods it moved until they recover.
package drain

import (
	"context"
	"errors"
	"fmt"
	"io"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	kubectldrain "k8s.io/kubectl/pkg/drain"
)

// Options are the kubectl drain settings an administrator can change.
type Options struct {
	// DeleteEmptyDirData is kubectl drain --delete-emptydir-data.
	DeleteEmptyDirData bool
	// Force is kubectl drain --force: it moves pods with no controller.
	Force bool
}

// Workload is a Deployment, StatefulSet, or ReplicaSet whose pods a drain
// moves, with how many of its pods were available before the drain.
type Workload struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Available int32  `json:"available"`
}

// Drainer drains nodes.
type Drainer struct {
	Client kubernetes.Interface
}

// helper returns kubectl drain's helper with the rules of
// `kubectl drain --ignore-daemonsets`, each pod's own grace period, and opts.
func (d *Drainer) helper(ctx context.Context, opts Options, dryRun bool) *kubectldrain.Helper {
	h := &kubectldrain.Helper{
		Ctx:                 ctx,
		Client:              d.Client,
		Force:               opts.Force,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  opts.DeleteEmptyDirData,
		Out:                 io.Discard,
		ErrOut:              io.Discard,
	}
	if dryRun {
		h.DryRunStrategy = cmdutil.DryRunServer
	}

	return h
}

// podsToMove returns the pods a drain of the node would move, or why the
// drain rules refuse it.
func (d *Drainer) podsToMove(h *kubectldrain.Helper, node string) ([]corev1.Pod, error) {
	list, errs := h.GetPodsForDeletion(node)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return list.Pods(), nil
}

// Check returns why the node can't be drained now, or nil if it can: the drain
// rules, then a dry-run eviction of each pod a drain would move, which a
// PodDisruptionBudget can refuse.
func (d *Drainer) Check(ctx context.Context, node string, opts Options) error {
	h := d.helper(ctx, opts, true)

	pods, err := d.podsToMove(h, node)
	if err != nil {
		return err
	}

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}

		if err := h.EvictPod(pod, policyv1.SchemeGroupVersion); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

// Evict asks once to evict each pod a drain moves, and returns how many are
// still on the node, including those shutting down. Evictions a
// PodDisruptionBudget refuses are asked again at the next call.
func (d *Drainer) Evict(ctx context.Context, node string, opts Options) (int, error) {
	h := d.helper(ctx, opts, false)

	pods, err := d.podsToMove(h, node)
	if err != nil {
		return 0, err
	}

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}

		err := h.EvictPod(pod, policyv1.SchemeGroupVersion)
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsTooManyRequests(err) {
			return 0, fmt.Errorf("evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return len(pods), nil
}

// Workloads returns the Deployments, StatefulSets, and ReplicaSets whose pods
// a drain of the node would move, with how many of their pods are available
// now. Pods of other controllers aren't tracked, nor are pods the drain rules
// refuse to move: those stop the drain itself.
func (d *Drainer) Workloads(ctx context.Context, node string, opts Options) ([]Workload, error) {
	list, errs := d.helper(ctx, opts, true).GetPodsForDeletion(node)
	if list == nil {
		return nil, errors.Join(errs...)
	}

	var workloads []Workload

	seen := map[Workload]bool{}

	for _, pod := range list.Pods() {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			continue
		}

		workload, ok, err := d.workloadOf(ctx, pod.Namespace, owner)
		if err != nil {
			return nil, err
		}

		key := Workload{Kind: workload.Kind, Namespace: workload.Namespace, Name: workload.Name}
		if !ok || seen[key] {
			continue
		}

		seen[key] = true

		workloads = append(workloads, workload)
	}

	return workloads, nil
}

// workloadOf returns the workload a pod's controller belongs to: a
// ReplicaSet's Deployment if it has one.
func (d *Drainer) workloadOf(ctx context.Context, namespace string, owner *metav1.OwnerReference) (Workload, bool, error) {
	switch owner.Kind {
	case "ReplicaSet":
		rs, err := d.Client.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return Workload{}, false, nil
		}

		if err != nil {
			return Workload{}, false, fmt.Errorf("get ReplicaSet %s/%s: %w", namespace, owner.Name, err)
		}

		if deployment := metav1.GetControllerOf(rs); deployment != nil && deployment.Kind == "Deployment" {
			return d.workloadOf(ctx, namespace, deployment)
		}

		return Workload{Kind: owner.Kind, Namespace: namespace, Name: owner.Name, Available: rs.Status.AvailableReplicas}, true, nil
	case "Deployment", "StatefulSet":
		available, _, found, err := d.availability(ctx, Workload{Kind: owner.Kind, Namespace: namespace, Name: owner.Name})
		if err != nil || !found {
			return Workload{}, false, err
		}

		return Workload{Kind: owner.Kind, Namespace: namespace, Name: owner.Name, Available: available}, true, nil
	default:
		return Workload{}, false, nil
	}
}

// Recovered reports whether every workload has as many pods available as it
// had before the drain, or as many as it now wants, if that's fewer. A
// workload that no longer exists has recovered.
func (d *Drainer) Recovered(ctx context.Context, workloads []Workload) (bool, error) {
	for _, workload := range workloads {
		available, wanted, found, err := d.availability(ctx, workload)
		if err != nil {
			return false, err
		}

		if found && available < min(workload.Available, wanted) {
			return false, nil
		}
	}

	return true, nil
}

// availability returns how many of a workload's pods are available, and how
// many it wants.
func (d *Drainer) availability(ctx context.Context, workload Workload) (available, wanted int32, found bool, err error) {
	apps := d.Client.AppsV1()

	var replicas *int32

	switch workload.Kind {
	case "Deployment":
		var obj *appsv1.Deployment

		obj, err = apps.Deployments(workload.Namespace).Get(ctx, workload.Name, metav1.GetOptions{})
		if err == nil {
			available, replicas = obj.Status.AvailableReplicas, obj.Spec.Replicas
		}
	case "StatefulSet":
		var obj *appsv1.StatefulSet

		obj, err = apps.StatefulSets(workload.Namespace).Get(ctx, workload.Name, metav1.GetOptions{})
		if err == nil {
			available, replicas = obj.Status.AvailableReplicas, obj.Spec.Replicas
		}
	case "ReplicaSet":
		var obj *appsv1.ReplicaSet

		obj, err = apps.ReplicaSets(workload.Namespace).Get(ctx, workload.Name, metav1.GetOptions{})
		if err == nil {
			available, replicas = obj.Status.AvailableReplicas, obj.Spec.Replicas
		}
	default:
		return 0, 0, false, nil
	}

	if apierrors.IsNotFound(err) {
		return 0, 0, false, nil
	}

	if err != nil {
		return 0, 0, false, fmt.Errorf("get %s %s/%s: %w", workload.Kind, workload.Namespace, workload.Name, err)
	}

	// A workload that doesn't set its replicas wants one, as the API defaults
	// it.
	wanted = 1
	if replicas != nil {
		wanted = *replicas
	}

	return available, wanted, true, nil
}
