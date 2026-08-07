// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type podState string

const (
	podStateCreating    podState = "creating"
	podStateImagePull   podState = "image-pull"
	podStateRunning     podState = "running"
	podStateCompleted   podState = "completed"
	podStateFailed      podState = "failed"
	podStateUnscheduled podState = "unscheduled"
	podStateOther       podState = "other"
)

type podStateCounts struct {
	Creating    int
	ImagePull   int
	Running     int
	Completed   int
	Failed      int
	Unscheduled int
	Other       int
}

type podStateTracker struct {
	mu     sync.RWMutex
	states map[string]podState
	err    error
}

func loadKubeConfig(path string) (clientcmd.ClientConfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}), nil
}

func newPodStateTracker(ctx context.Context, kubeconfig, namespace, jobName string) (*podStateTracker, error) {
	clientConfig, err := loadKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes client config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	tracker := &podStateTracker{states: map[string]podState{}}
	if err := tracker.replaceFromList(ctx, client, namespace, jobName); err != nil {
		return nil, err
	}

	go tracker.run(ctx, client, namespace, jobName)

	return tracker, nil
}

func (t *podStateTracker) replaceFromList(ctx context.Context, client kubernetes.Interface, namespace, jobName string) error {
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return fmt.Errorf("list pull Job pods: %w", err)
	}

	states := make(map[string]podState, len(list.Items))
	for index := range list.Items {
		pod := &list.Items[index]
		states[string(pod.UID)] = classifyPod(pod)
	}

	t.mu.Lock()
	t.states = states
	t.err = nil
	t.mu.Unlock()

	return nil
}

func (t *podStateTracker) run(ctx context.Context, client kubernetes.Interface, namespace, jobName string) {
	for ctx.Err() == nil {
		list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
		if err != nil {
			t.setError(err)

			if !waitForRetry(ctx) {
				return
			}

			continue
		}

		states := make(map[string]podState, len(list.Items))
		for index := range list.Items {
			pod := &list.Items[index]
			states[string(pod.UID)] = classifyPod(pod)
		}

		t.replace(states)

		watcher, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
			LabelSelector:       "job-name=" + jobName,
			ResourceVersion:     list.ResourceVersion,
			AllowWatchBookmarks: true,
		})
		if err != nil {
			t.setError(err)

			if !waitForRetry(ctx) {
				return
			}

			continue
		}

		closed := t.consumeWatch(ctx, watcher)
		watcher.Stop()

		if !closed {
			return
		}
	}
}

func (t *podStateTracker) consumeWatch(ctx context.Context, watcher watch.Interface) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return true
			}

			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				if event.Type == watch.Error {
					t.setError(fmt.Errorf("pod watch returned an error event"))
					return true
				}

				continue
			}

			key := string(pod.UID)

			t.mu.Lock()
			if event.Type == watch.Deleted {
				delete(t.states, key)
			} else {
				t.states[key] = classifyPod(pod)
			}

			t.err = nil
			t.mu.Unlock()
		}
	}
}

func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *podStateTracker) replace(states map[string]podState) {
	t.mu.Lock()
	t.states = states
	t.err = nil
	t.mu.Unlock()
}

func (t *podStateTracker) setError(err error) {
	t.mu.Lock()
	t.err = err
	t.mu.Unlock()
}

func (t *podStateTracker) snapshot() (podStateCounts, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	counts := podStateCounts{}

	for _, state := range t.states {
		switch state {
		case podStateCreating:
			counts.Creating++
		case podStateImagePull:
			counts.ImagePull++
		case podStateRunning:
			counts.Running++
		case podStateCompleted:
			counts.Completed++
		case podStateFailed:
			counts.Failed++
		case podStateUnscheduled:
			counts.Unscheduled++
		case podStateOther:
			counts.Other++
		}
	}

	return counts, t.err
}

func classifyPod(pod *corev1.Pod) podState {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return podStateCompleted
	case corev1.PodFailed:
		return podStateFailed
	case corev1.PodRunning:
		return podStateRunning
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "pull" || status.State.Waiting == nil {
			continue
		}

		switch status.State.Waiting.Reason {
		case "ImagePullBackOff", "ErrImagePull", "RegistryUnavailable":
			return podStateImagePull
		case "ContainerCreating", "PodInitializing":
			return podStateCreating
		default:
			return podStateOther
		}
	}

	if pod.Spec.NodeName == "" {
		return podStateUnscheduled
	}

	return podStateCreating
}
