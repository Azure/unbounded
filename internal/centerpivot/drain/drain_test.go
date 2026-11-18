// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const node = "acl-1"

func owned(kind, name string) []metav1.OwnerReference {
	apiVersion := "apps/v1"

	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: ptr.To(true)}}
}

func pod(name string, owners []metav1.OwnerReference, emptyDir bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", OwnerReferences: owners},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if emptyDir {
		p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	}

	return p
}

// cluster returns a node running a Deployment's pod, a StatefulSet's pod, and
// a DaemonSet's pod, which a drain leaves.
func cluster(extra ...runtime.Object) *fake.Clientset {
	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](3)},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 3},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps", OwnerReferences: owned("Deployment", "web")},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "apps"},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](2)},
			Status:     appsv1.StatefulSetStatus{AvailableReplicas: 2},
		},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "apps"}},
		pod("web-1-a", owned("ReplicaSet", "web-1"), false),
		pod("db-0", owned("StatefulSet", "db"), false),
		pod("agent-x", owned("DaemonSet", "agent"), false),
	}

	return fake.NewClientset(append(objects, extra...)...)
}

// evictionLog records evictions, and refuses those of the pods in blocked as
// a PodDisruptionBudget would. A real eviction deletes the pod.
type evictionLog struct {
	blocked map[string]bool
	evicted []string
}

func evictions(client *fake.Clientset, blocked ...string) *evictionLog {
	log := &evictionLog{blocked: map[string]bool{}}
	for _, name := range blocked {
		log.blocked[name] = true
	}

	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetSubresource() != "eviction" {
			return false, nil, nil
		}

		eviction, ok := create.GetObject().(*policyv1.Eviction)
		if !ok {
			return false, nil, nil
		}

		if log.blocked[eviction.Name] {
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
		}

		if len(eviction.DeleteOptions.DryRun) > 0 {
			return true, nil, nil
		}

		log.evicted = append(log.evicted, eviction.Name)

		return true, nil, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), eviction.Namespace, eviction.Name)
	})

	return log
}

func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		extra   []runtime.Object
		opts    Options
		blocked []string
		wantErr string
	}{
		{name: "pods that can move"},
		{name: "a budget refuses", blocked: []string{"db-0"}, wantErr: "db-0"},
		{name: "emptyDir data", extra: []runtime.Object{pod("cache", owned("ReplicaSet", "web-1"), true)}, wantErr: "local storage"},
		{name: "emptyDir data allowed", extra: []runtime.Object{pod("cache", owned("ReplicaSet", "web-1"), true)}, opts: Options{DeleteEmptyDirData: true}},
		{name: "a pod with no controller", extra: []runtime.Object{pod("lonely", nil, false)}, wantErr: "lonely"},
		{name: "a pod with no controller allowed", extra: []runtime.Object{pod("lonely", nil, false)}, opts: Options{Force: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := cluster(tt.extra...)
			log := evictions(client, tt.blocked...)

			err := (&Drainer{Client: client}).Check(t.Context(), node, tt.opts)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			assert.Empty(t, log.evicted, "a check moves nothing")
		})
	}
}

func TestEvictUntilTheNodeIsEmpty(t *testing.T) {
	t.Parallel()

	client := cluster()
	log := evictions(client, "db-0")
	drainer := &Drainer{Client: client}

	left, err := drainer.Evict(t.Context(), node, Options{})
	require.NoError(t, err)
	assert.Equal(t, 2, left, "pods count until they're gone")
	assert.Equal(t, []string{"web-1-a"}, log.evicted, "the budget refused db-0, and DaemonSet pods stay")

	delete(log.blocked, "db-0")

	left, err = drainer.Evict(t.Context(), node, Options{})
	require.NoError(t, err)
	assert.Equal(t, 1, left)

	left, err = drainer.Evict(t.Context(), node, Options{})
	require.NoError(t, err)
	assert.Equal(t, 0, left)
	assert.Equal(t, []string{"web-1-a", "db-0"}, log.evicted)
}

func TestWorkloadsRecover(t *testing.T) {
	t.Parallel()

	client := cluster()
	drainer := &Drainer{Client: client}

	workloads, err := drainer.Workloads(t.Context(), node, Options{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []Workload{
		{Kind: "Deployment", Namespace: "apps", Name: "web", Available: 3},
		{Kind: "StatefulSet", Namespace: "apps", Name: "db", Available: 2},
	}, workloads, "a ReplicaSet's pods count for its Deployment; DaemonSets aren't tracked")

	setAvailable := func(available int32) {
		deployment, err := client.AppsV1().Deployments("apps").Get(t.Context(), "web", metav1.GetOptions{})
		require.NoError(t, err)

		deployment.Status.AvailableReplicas = available
		_, err = client.AppsV1().Deployments("apps").UpdateStatus(t.Context(), deployment, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	setAvailable(2)

	recovered, err := drainer.Recovered(t.Context(), workloads)
	require.NoError(t, err)
	assert.False(t, recovered)

	setAvailable(3)

	recovered, err = drainer.Recovered(t.Context(), workloads)
	require.NoError(t, err)
	assert.True(t, recovered)

	require.NoError(t, client.AppsV1().StatefulSets("apps").Delete(t.Context(), "db", metav1.DeleteOptions{}))

	recovered, err = drainer.Recovered(t.Context(), workloads)
	require.NoError(t, err)
	assert.True(t, recovered, "a workload that's gone has recovered")
}
