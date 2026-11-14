// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

func reclaimResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

func RunReclaim(ctx context.Context, client kubernetes.Interface, spec ReclaimSpec, emit func(ReclaimProgress)) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	job, err := client.BatchV1().Jobs(spec.Namespace).Get(ctx, reclaimJobName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	saved, err := ownedReclaim(job)
	if err != nil {
		return err
	}

	if saved != spec || job.DeletionTimestamp != nil {
		return errors.New("reclaim configuration changed or was canceled")
	}

	progress := ReclaimProgress{Stage: "deleting", Message: "Deleting dashboard-managed demo DaemonSets"}
	reportFailure := func(err error) error {
		progress.Stage = "failed"
		progress.Message = err.Error()
		emit(progress)

		return err
	}

	emit(progress)

	builds, err := client.BatchV1().Jobs(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managerLabel + "=gantry-ui," + buildLabel})
	if err != nil {
		return reportFailure(err)
	}

	for _, build := range builds.Items {
		if !buildFinished(&build) {
			return reportFailure(errors.New("an image build is active; no demos were deleted"))
		}
	}

	workloads, err := client.AppsV1().DaemonSets(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managerLabel + "=gantry-ui"})
	if err != nil {
		return reportFailure(err)
	}

	demos := make([]Deployment, 0, len(workloads.Items))
	digests := map[string]bool{}
	// Validate the full deletion set before mutating any workload.
	for _, workload := range workloads.Items {
		demo, err := describeDeployment(&workload)
		if err != nil {
			return reportFailure(err)
		}

		if demo.UID == "" {
			return reportFailure(errors.New("demo DaemonSet has no identity"))
		}

		demos = append(demos, demo)

		digests[demo.Image.Digest] = true
	}

	for _, demo := range demos {
		uid := types.UID(demo.UID)
		if err := client.AppsV1().DaemonSets(spec.Namespace).Delete(ctx, demo.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
		}); err != nil {
			return reportFailure(fmt.Errorf("delete demo %s: %w", demo.Name, err))
		}

		progress.Deleted++
		emit(progress)
	}

	progress.Message = "Waiting for deleted demo Pods to terminate; no force deletion"
	emit(progress)

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := waitForDemoDeletion(waitCtx, client, spec.Namespace, demos); err != nil {
		return reportFailure(err)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.azure.com/agentpool=" + spec.NodePool + ",kubernetes.azure.com/nodepool-type=FlexNodes,kubernetes.io/os=linux,kubernetes.io/arch=" + spec.Architecture})
	if err != nil {
		return reportFailure(err)
	}

	if len(nodes.Items) == 0 {
		return reportFailure(errors.New("no matching Flex workers; no cache cleanup performed"))
	}

	progress.Total = len(nodes.Items)
	progress.Stage = "reclaiming"
	progress.Message = "Reclaiming unused images; active containers, pinned images, and uncertain leases are protected"
	emit(progress)

	deletedDigests := make([]string, 0, len(digests))
	for digest := range digests {
		deletedDigests = append(deletedDigests, digest)
	}

	var mu sync.Mutex

	group := new(errgroup.Group)
	group.SetLimit(64)

	for _, node := range nodes.Items {
		group.Go(func() error {
			result, err := runReclaimWorker(ctx, client, job, spec, node, deletedDigests)

			mu.Lock()
			defer mu.Unlock()

			progress.Completed++
			if err != nil {
				progress.Failed++
				progress.Message = fmt.Sprintf("%s: %v", node.Name, err)
			} else {
				if result.Before > result.After {
					progress.Reclaimed += result.Before - result.After
				}

				progress.ProtectedLeases += result.ProtectedLeases
			}

			emit(progress)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return reportFailure(err)
	}

	if progress.Failed > 0 {
		return reportFailure(fmt.Errorf("%d of %d workers could not finish reclamation; retry after resolving their reported errors", progress.Failed, progress.Total))
	}

	if ctx.Err() != nil {
		return reportFailure(ctx.Err())
	}

	progress.Stage = "complete"

	progress.Message = "Demo workloads removed and unused image cleanup verified on all workers"
	if progress.ProtectedLeases > 0 {
		progress.Message += "; runtime leases remain protected, so this is not a full disk wipe"
	}

	emit(progress)

	return nil
}

func waitForDemoDeletion(ctx context.Context, client kubernetes.Interface, namespace string, demos []Deployment) error {
	for {
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}

		remaining := false

		for _, pod := range pods.Items {
			for _, demo := range demos {
				if ownedPod(&pod, demo) {
					remaining = true
					break
				}
			}
		}

		if !remaining {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("demo Pods have not terminated: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func reclaimWorkerJob(owner *batchv1.Job, spec ReclaimSpec, node corev1.Node, digests []string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "gantry-reclaim-node-", Namespace: spec.Namespace,
			Labels:          map[string]string{managerLabel: "gantry-ui", reclaimLabel: "worker"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: owner.Name, UID: owner.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false)}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(900)), TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{reclaimLabel: "worker"}},
				Spec: corev1.PodSpec{
					NodeName: node.Name, AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
					ImagePullSecrets: helperPullSecrets(spec.ImagePullSecret),
					Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name: "reclaim-node", Image: spec.Image, ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/unbounded/bin/gantry-ui", "reclaim-node"},
						Env:             []corev1.EnvVar{{Name: "GANTRY_RECLAIM_NODE", Value: node.Name}, {Name: "GANTRY_RECLAIM_DIGESTS", Value: strings.Join(digests, ",")}},
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true), RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0)), ReadOnlyRootFilesystem: ptr.To(true)},
						Resources:       reclaimResources(),
						VolumeMounts:    []corev1.VolumeMount{{Name: "runtime", MountPath: "/run/containerd"}, {Name: "storage", MountPath: "/var/lib/gantry", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{
						{Name: "runtime", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd", Type: ptr.To(corev1.HostPathDirectory)}}},
						{Name: "storage", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/gantry", Type: ptr.To(corev1.HostPathDirectory)}}},
					},
				},
			},
		},
	}
}

func runReclaimWorker(ctx context.Context, client kubernetes.Interface, owner *batchv1.Job, spec ReclaimSpec, node corev1.Node, digests []string) (ReclaimNodeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 16*time.Minute)
	defer cancel()

	created, err := client.BatchV1().Jobs(spec.Namespace).Create(ctx, reclaimWorkerJob(owner, spec, node, digests), metav1.CreateOptions{})
	if err != nil {
		return ReclaimNodeResult{}, err
	}

	for {
		job, err := client.BatchV1().Jobs(spec.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return ReclaimNodeResult{}, err
		}

		if job.UID != created.UID {
			return ReclaimNodeResult{}, errors.New("worker Job identity changed")
		}

		pods, err := client.CoreV1().Pods(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + created.Name})
		if err != nil {
			return ReclaimNodeResult{}, err
		}

		for _, pod := range pods.Items {
			if !jobOwnsPod(created, &pod) || pod.Spec.NodeName != node.Name {
				continue
			}

			for _, container := range pod.Status.ContainerStatuses {
				if container.Name != "reclaim-node" || container.State.Terminated == nil {
					continue
				}

				terminated := container.State.Terminated
				if terminated.ExitCode != 0 {
					return ReclaimNodeResult{}, fmt.Errorf("node cleanup failed: %s", terminated.Message)
				}

				var result ReclaimNodeResult
				if err := json.Unmarshal([]byte(terminated.Message), &result); err != nil {
					return result, fmt.Errorf("invalid worker result: %w", err)
				}

				if result.Node != node.Name {
					return result, errors.New("worker result does not match target node")
				}

				return result, nil
			}
		}

		if buildFinished(job) {
			return ReclaimNodeResult{}, errors.New("worker Job failed without a node result; inspect its Pod events")
		}

		select {
		case <-ctx.Done():
			return ReclaimNodeResult{}, fmt.Errorf("worker deadline: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}
