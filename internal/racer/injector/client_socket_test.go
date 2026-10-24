// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testClientDir = "/run/racer/" + testCache + "/client"

// newClientPod returns a pod opted into the client socket only.
func newClientPod(annotations map[string]string) *corev1.Pod {
	a := map[string]string{ClientCacheAnnotation: testCache}

	for k, v := range annotations {
		if v == "<delete>" {
			delete(a, k)
			continue
		}

		a[k] = v
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Labels: map[string]string{ClientSocketLabel: EnabledValue}, Annotations: a},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "setup", Image: "app"}},
			Containers:     []corev1.Container{{Name: "app", Image: "app"}, {Name: "helper", Image: "app"}},
		},
	}
}

func socketMounted(c corev1.Container, volume string) bool {
	mount := false

	for _, m := range c.VolumeMounts {
		if reflect.DeepEqual(m, corev1.VolumeMount{Name: volume, MountPath: testClientDir, ReadOnly: true}) {
			mount = true
		}
	}

	env := false

	for _, e := range c.Env {
		if e.Name == SocketEnv && e.Value == testClientDir+"/socket" {
			env = true
		}
	}

	return mount && env
}

func TestMutateClientSocketAllContainers(t *testing.T) {
	pod := newClientPod(nil)
	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	for _, c := range pod.Spec.Containers {
		if !socketMounted(c, ClientVolumeName) {
			t.Fatalf("container %s: %+v", c.Name, c)
		}
	}

	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.InitContainers[0].VolumeMounts) != 0 || len(pod.Spec.InitContainers[0].Env) != 0 {
		t.Fatalf("init containers must be untouched and no sidecar added: %+v", pod.Spec.InitContainers)
	}

	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("volumes: %+v", pod.Spec.Volumes)
	}

	v := pod.Spec.Volumes[0]
	if v.Name != ClientVolumeName || v.HostPath.Path != testClientDir || *v.HostPath.Type != corev1.HostPathDirectory {
		t.Fatalf("volume: %+v", v)
	}

	if pod.Spec.NodeSelector[osLabel] != "linux" || pod.Spec.Affinity == nil {
		t.Fatalf("placement: %+v %+v", pod.Spec.NodeSelector, pod.Spec.Affinity)
	}

	if pod.Annotations[ClientSocketInjectedAnnotation] != testCache || pod.Annotations[InjectedAnnotation] != "" {
		t.Fatalf("annotations: %+v", pod.Annotations)
	}

	// A second admission pass must not change anything.
	before := pod.DeepCopy()
	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(before, pod) {
		t.Fatal("second Mutate changed the pod")
	}
}

func TestMutateClientSocketSelectedContainers(t *testing.T) {
	pod := newClientPod(map[string]string{ClientContainersAnnotation: " helper "})
	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	if socketMounted(pod.Spec.Containers[0], ClientVolumeName) || !socketMounted(pod.Spec.Containers[1], ClientVolumeName) {
		t.Fatalf("containers: %+v", pod.Spec.Containers)
	}
}

func TestMutateClientSocketWithSidecarSharesVolume(t *testing.T) {
	pod := newPod(nil)
	pod.Labels[ClientSocketLabel] = EnabledValue
	pod.Annotations[ClientCacheAnnotation] = testCache

	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != VolumeName {
		t.Fatalf("want one shared volume, got %+v", pod.Spec.Volumes)
	}

	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != ContainerName {
		t.Fatalf("sidecar missing: %+v", pod.Spec.InitContainers)
	}

	if !socketMounted(pod.Spec.Containers[0], VolumeName) {
		t.Fatalf("app container: %+v", pod.Spec.Containers[0])
	}

	// Distinct caches get distinct volumes.
	pod = newPod(nil)
	pod.Labels[ClientSocketLabel] = EnabledValue
	pod.Annotations[ClientCacheAnnotation] = "other"

	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	if len(pod.Spec.Volumes) != 2 || pod.Spec.Volumes[1].Name != ClientVolumeName || pod.Spec.Volumes[1].HostPath.Path != "/run/racer/other/client" {
		t.Fatalf("volumes: %+v", pod.Spec.Volumes)
	}
}

func TestMutateClientSocketHostNetworkAllowed(t *testing.T) {
	pod := newClientPod(nil)
	pod.Spec.HostNetwork = true

	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}
}

func TestMutateClientSocketNoop(t *testing.T) {
	for name, modify := range map[string]func(*corev1.Pod){
		"unlabeled":      func(p *corev1.Pod) { p.Labels = nil; p.Annotations = nil },
		"label disabled": func(p *corev1.Pod) { p.Labels[ClientSocketLabel] = "disabled" },
		"already injected": func(p *corev1.Pod) {
			p.Annotations = map[string]string{ClientSocketInjectedAnnotation: testCache}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := newClientPod(nil)
			modify(pod)
			before := pod.DeepCopy()

			if err := Mutate(pod, testCfg); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(before, pod) {
				t.Fatal("pod changed")
			}
		})
	}
}

func TestMutateClientSocketErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		anno   map[string]string
		modify func(*corev1.Pod)
		want   string
	}{
		{name: "missing cache", anno: map[string]string{ClientCacheAnnotation: "<delete>"}, want: ClientCacheAnnotation},
		{name: "invalid cache", anno: map[string]string{ClientCacheAnnotation: "../x"}, want: ClientCacheAnnotation},
		{name: "unknown container", anno: map[string]string{ClientContainersAnnotation: "nope"}, want: ClientContainersAnnotation},
		{name: "init container", anno: map[string]string{ClientContainersAnnotation: "setup"}, want: ClientContainersAnnotation},
		{name: "duplicate container", anno: map[string]string{ClientContainersAnnotation: "app,app"}, want: ClientContainersAnnotation},
		{name: "empty list", anno: map[string]string{ClientContainersAnnotation: ""}, want: ClientContainersAnnotation},
		{
			name:   "volume name taken",
			modify: func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: ClientVolumeName}} },
			want:   ClientVolumeName,
		},
		{
			name: "mount path taken",
			modify: func(p *corev1.Pod) {
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "x", MountPath: testClientDir}}
			},
			want: "already mounts",
		},
		{
			name:   "env taken",
			modify: func(p *corev1.Pod) { p.Spec.Containers[1].Env = []corev1.EnvVar{{Name: SocketEnv, Value: "x"}} },
			want:   SocketEnv,
		},
		{
			name:   "windows os",
			modify: func(p *corev1.Pod) { p.Spec.OS = &corev1.PodOS{Name: corev1.Windows} },
			want:   "spec.os.name",
		},
		{
			name:   "windows node selector",
			modify: func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{osLabel: "windows"} },
			want:   osLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newClientPod(tc.anno)
			if tc.modify != nil {
				tc.modify(pod)
			}

			before := pod.DeepCopy()

			err := Mutate(pod, testCfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}

			if !reflect.DeepEqual(before, pod) {
				t.Fatal("pod changed on error")
			}
		})
	}
}
