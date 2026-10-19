// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const testImage = "example.com/racer-object:v1"

var testCfg = Config{Image: testImage}

func newPod(annotations map[string]string) *corev1.Pod {
	a := map[string]string{NamespaceAnnotation: "store", BucketsAnnotation: "one"}

	for k, v := range annotations {
		if v == "<delete>" {
			delete(a, k)
			continue
		}

		a[k] = v
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Labels: map[string]string{EnabledLabel: EnabledValue}, Annotations: a},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app"}}},
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("expected error for empty image")
	}

	if err := (Config{Image: " "}).Validate(); err == nil {
		t.Fatal("expected error for blank image")
	}

	if err := testCfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := Mutate(newPod(nil), Config{}); err == nil {
		t.Fatal("expected Mutate to reject empty image")
	}
}

func TestMutateSuccess(t *testing.T) {
	tests := []struct {
		name      string
		anno      map[string]string
		wantArgs  []string
		wantCache string
	}{
		{"minimal", nil, []string{"sidecar", "--cache=racer-object", "--namespace=store", "--bucket=one"}, "racer-object"},
		{
			"full s3",
			map[string]string{BucketsAnnotation: "one,two", CacheAnnotation: "my.cache", APIAnnotation: "s3", ListenAnnotation: "127.0.0.1:9000"},
			[]string{"sidecar", "--cache=my.cache", "--namespace=store", "--bucket=one", "--bucket=two", "--api=s3", "--listen=127.0.0.1:9000"},
			"my.cache",
		},
		{
			"full azure ipv6",
			map[string]string{APIAnnotation: "azure", ListenAnnotation: "[::1]:8081"},
			[]string{"sidecar", "--cache=racer-object", "--namespace=store", "--bucket=one", "--api=azure", "--listen=[::1]:8081"},
			"racer-object",
		},
		{
			"bucket whitespace",
			map[string]string{BucketsAnnotation: " a , b ,c "},
			[]string{"sidecar", "--cache=racer-object", "--namespace=store", "--bucket=a", "--bucket=b", "--bucket=c"},
			"racer-object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(tt.anno)
			if err := Mutate(pod, testCfg); err != nil {
				t.Fatal(err)
			}

			if len(pod.Spec.InitContainers) != 1 {
				t.Fatalf("init containers: %+v", pod.Spec.InitContainers)
			}

			c := pod.Spec.InitContainers[0]
			if c.Name != ContainerName || c.Image != testImage || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
				t.Fatalf("container: %+v", c)
			}

			if !reflect.DeepEqual(c.Args, tt.wantArgs) {
				t.Fatalf("args = %v, want %v", c.Args, tt.wantArgs)
			}

			dir := "/run/racer/" + tt.wantCache + "/client"
			if len(c.VolumeMounts) != 1 || !reflect.DeepEqual(c.VolumeMounts[0], corev1.VolumeMount{Name: VolumeName, MountPath: dir, ReadOnly: true}) {
				t.Fatalf("mounts: %+v", c.VolumeMounts)
			}

			sc := c.SecurityContext
			if *sc.RunAsUser != SidecarUID || *sc.RunAsGroup != SidecarUID || !*sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
				!reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatalf("security context: %+v", sc)
			}

			if c.Resources.Requests != nil || c.Resources.Limits != nil || c.LivenessProbe != nil || c.ReadinessProbe != nil || len(c.Ports) != 0 {
				t.Fatalf("unexpected resources/probes/ports: %+v", c)
			}

			v := pod.Spec.Volumes[len(pod.Spec.Volumes)-1]
			if v.Name != VolumeName || v.HostPath.Path != dir || *v.HostPath.Type != corev1.HostPathDirectory {
				t.Fatalf("volume: %+v", v)
			}

			if pod.Spec.NodeSelector["kubernetes.io/os"] != "linux" {
				t.Fatal("missing os nodeSelector")
			}

			terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			if len(terms) != 1 || !reflect.DeepEqual(terms[0].MatchExpressions, []corev1.NodeSelectorRequirement{{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist}}) {
				t.Fatalf("terms: %+v", terms)
			}

			if pod.Annotations[InjectedAnnotation] != testImage {
				t.Fatal("missing injected annotation")
			}
		})
	}
}

func TestMutateErrors(t *testing.T) {
	tests := []struct {
		name   string
		anno   map[string]string
		modify func(*corev1.Pod)
		want   string
	}{
		{name: "missing namespace", anno: map[string]string{NamespaceAnnotation: "<delete>"}, want: NamespaceAnnotation},
		{name: "empty namespace", anno: map[string]string{NamespaceAnnotation: ""}, want: NamespaceAnnotation},
		{name: "missing buckets", anno: map[string]string{BucketsAnnotation: "<delete>"}, want: BucketsAnnotation},
		{name: "empty bucket entry", anno: map[string]string{BucketsAnnotation: "a,,b"}, want: BucketsAnnotation},
		{name: "trailing comma", anno: map[string]string{BucketsAnnotation: "a, "}, want: BucketsAnnotation},
		{name: "invalid bucket", anno: map[string]string{BucketsAnnotation: "a/b"}, want: BucketsAnnotation},
		{name: "bad api", anno: map[string]string{APIAnnotation: "gcs"}, want: APIAnnotation},
		{name: "empty api", anno: map[string]string{APIAnnotation: ""}, want: APIAnnotation},
		{name: "non-loopback listen", anno: map[string]string{ListenAnnotation: "0.0.0.0:8080"}, want: ListenAnnotation},
		{name: "hostname listen", anno: map[string]string{ListenAnnotation: "localhost:8080"}, want: ListenAnnotation},
		{name: "no port listen", anno: map[string]string{ListenAnnotation: "127.0.0.1"}, want: ListenAnnotation},
		{name: "bad port listen", anno: map[string]string{ListenAnnotation: "127.0.0.1:70000"}, want: ListenAnnotation},
		{name: "cache traversal", anno: map[string]string{CacheAnnotation: "../x"}, want: CacheAnnotation},
		{name: "cache slash", anno: map[string]string{CacheAnnotation: "a/b"}, want: CacheAnnotation},
		{name: "cache uppercase", anno: map[string]string{CacheAnnotation: "Abc"}, want: CacheAnnotation},
		{name: "cache empty", anno: map[string]string{CacheAnnotation: ""}, want: CacheAnnotation},
		{
			name: "volume collision", want: VolumeName,
			modify: func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: VolumeName}} },
		},
		{
			name: "conflicting os", want: "kubernetes.io/os",
			modify: func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "windows"} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(tt.anno)
			if tt.modify != nil {
				tt.modify(pod)
			}

			err := Mutate(pod, testCfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestMutatePodOS(t *testing.T) {
	tests := []struct {
		name         string
		os           *corev1.PodOS
		nodeSelector map[string]string
		want         string
	}{
		{name: "nil os", os: nil},
		{name: "linux os", os: &corev1.PodOS{Name: corev1.Linux}},
		{name: "windows os", os: &corev1.PodOS{Name: corev1.Windows}, want: "spec.os.name"},
		{name: "unknown os", os: &corev1.PodOS{Name: "plan9"}, want: "spec.os.name"},
		{name: "empty os name", os: &corev1.PodOS{}, want: "spec.os.name"},
		{
			name: "linux os with windows nodeSelector", os: &corev1.PodOS{Name: corev1.Linux},
			nodeSelector: map[string]string{osLabel: "windows"}, want: osLabel,
		},
		{
			name: "windows os with linux nodeSelector", os: &corev1.PodOS{Name: corev1.Windows},
			nodeSelector: map[string]string{osLabel: "linux"}, want: "spec.os.name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(nil)
			pod.Spec.OS = tt.os
			pod.Spec.NodeSelector = tt.nodeSelector
			original := pod.DeepCopy()

			err := Mutate(pod, testCfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Mutate() error = %v", err)
				}

				if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != ContainerName {
					t.Fatalf("sidecar not injected: %+v", pod.Spec.InitContainers)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}

			if !reflect.DeepEqual(pod, original) {
				t.Fatal("rejected pod was modified")
			}
		})
	}
}

func TestMutateResources(t *testing.T) {
	q := resource.MustParse

	tests := []struct {
		name     string
		anno     map[string]string
		requests corev1.ResourceList
		limits   corev1.ResourceList
		err      string
	}{
		{name: "cpu request", anno: map[string]string{CPURequestAnnotation: "100m"}, requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")}},
		{name: "cpu limit", anno: map[string]string{CPULimitAnnotation: "1"}, limits: corev1.ResourceList{corev1.ResourceCPU: q("1")}},
		{name: "memory request", anno: map[string]string{MemoryRequestAnnotation: "64Mi"}, requests: corev1.ResourceList{corev1.ResourceMemory: q("64Mi")}},
		{name: "memory limit", anno: map[string]string{MemoryLimitAnnotation: "1Gi"}, limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}},
		{
			name:     "all four",
			anno:     map[string]string{CPURequestAnnotation: "100m", CPULimitAnnotation: "100m", MemoryRequestAnnotation: "64Mi", MemoryLimitAnnotation: "1Gi"},
			requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("64Mi")},
			limits:   corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("1Gi")},
		},
		{name: "malformed", anno: map[string]string{CPURequestAnnotation: "lots"}, err: CPURequestAnnotation},
		{name: "zero", anno: map[string]string{MemoryLimitAnnotation: "0"}, err: MemoryLimitAnnotation},
		{name: "negative", anno: map[string]string{CPULimitAnnotation: "-1"}, err: CPULimitAnnotation},
		{name: "empty", anno: map[string]string{MemoryRequestAnnotation: ""}, err: MemoryRequestAnnotation},
		{name: "cpu request > limit", anno: map[string]string{CPURequestAnnotation: "2", CPULimitAnnotation: "1"}, err: CPURequestAnnotation},
		{name: "memory request > limit", anno: map[string]string{MemoryRequestAnnotation: "2Gi", MemoryLimitAnnotation: "1Gi"}, err: MemoryRequestAnnotation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(tt.anno)

			err := Mutate(pod, testCfg)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			r := pod.Spec.InitContainers[0].Resources
			if !equalList(r.Requests, tt.requests) || !equalList(r.Limits, tt.limits) {
				t.Fatalf("resources = %+v", r)
			}
		})
	}
}

func equalList(a, b corev1.ResourceList) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}

	for k, v := range b {
		if got, ok := a[k]; !ok || got.Cmp(v) != 0 {
			return false
		}
	}

	return true
}

func TestMutatePlacementAndOrdering(t *testing.T) {
	pod := newPod(nil)
	pod.Spec.InitContainers = []corev1.Container{{Name: "first"}, {Name: "second"}}
	pod.Spec.NodeSelector = map[string]string{"zone": "a", "kubernetes.io/os": "linux"}
	pref := []corev1.PreferredSchedulingTerm{{Weight: 1, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "x", Operator: corev1.NodeSelectorOpExists}}}}}
	podAnti := &corev1.PodAntiAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 2, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "t"}}}}
	excl := corev1.NodeSelectorRequirement{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist}
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "a", Operator: corev1.NodeSelectorOpExists}}},
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"n"}}}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{excl}},
			}},
			PreferredDuringSchedulingIgnoredDuringExecution: pref,
		},
		PodAntiAffinity: podAnti,
	}

	if err := Mutate(pod, testCfg); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}

	if !reflect.DeepEqual(names, []string{ContainerName, "first", "second"}) {
		t.Fatalf("order = %v", names)
	}

	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"zone": "a", "kubernetes.io/os": "linux"}) {
		t.Fatalf("nodeSelector = %v", pod.Spec.NodeSelector)
	}

	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 3 {
		t.Fatalf("terms = %+v", terms)
	}

	wantLens := []int{2, 1, 1}
	for i, term := range terms {
		if len(term.MatchExpressions) != wantLens[i] || !reflect.DeepEqual(term.MatchExpressions[len(term.MatchExpressions)-1], excl) {
			t.Fatalf("term %d = %+v", i, term)
		}
	}

	if len(terms[1].MatchFields) != 1 {
		t.Fatal("match fields lost")
	}

	if !reflect.DeepEqual(pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, pref) || pod.Spec.Affinity.PodAntiAffinity != podAnti {
		t.Fatal("preferred or pod anti-affinity changed")
	}
}

func TestMutateNoop(t *testing.T) {
	tests := map[string]func(*corev1.Pod){
		"unlabeled":      func(p *corev1.Pod) { p.Labels = nil },
		"label disabled": func(p *corev1.Pod) { p.Labels[EnabledLabel] = "disabled" },
		"existing init":  func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: ContainerName}} },
		"existing main": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: ContainerName})
		},
		"unlabeled invalid": func(p *corev1.Pod) { p.Labels = nil; p.Annotations = nil },
	}
	for name, modify := range tests {
		t.Run(name, func(t *testing.T) {
			pod := newPod(nil)
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

func request(t *testing.T, op admissionv1.Operation, raw []byte) admission.Request {
	t.Helper()

	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "uid",
		Operation: op,
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func TestHandler(t *testing.T) {
	h := NewHandler(testCfg)
	ctx := context.Background()

	pod := newPod(nil)

	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create patches", func(t *testing.T) {
		resp := h.Handle(ctx, request(t, admissionv1.Create, raw))
		if !resp.Allowed || len(resp.Patches) == 0 {
			t.Fatalf("resp = %+v", resp)
		}

		ops, err := json.Marshal(resp.Patches)
		if err != nil {
			t.Fatal(err)
		}

		patch, err := jsonpatch.DecodePatch(ops)
		if err != nil {
			t.Fatal(err)
		}

		out, err := patch.Apply(raw)
		if err != nil {
			t.Fatal(err)
		}

		var got corev1.Pod
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}

		want := pod.DeepCopy()
		if err := Mutate(want, testCfg); err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(&got, want) {
			t.Fatalf("patched pod mismatch:\n got %+v\nwant %+v", got.Spec, want.Spec)
		}
	})

	t.Run("denied", func(t *testing.T) {
		bad := newPod(map[string]string{APIAnnotation: "gcs"})

		b, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}

		resp := h.Handle(ctx, request(t, admissionv1.Create, b))
		if resp.Allowed || !strings.Contains(resp.Result.Message, APIAnnotation) {
			t.Fatalf("resp = %+v", resp)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		resp := h.Handle(ctx, request(t, admissionv1.Create, []byte("{")))
		if resp.Allowed || resp.Result.Code != http.StatusBadRequest {
			t.Fatalf("resp = %+v", resp)
		}
	})

	t.Run("update noop", func(t *testing.T) {
		resp := h.Handle(ctx, request(t, admissionv1.Update, raw))
		if !resp.Allowed || len(resp.Patches) != 0 {
			t.Fatalf("resp = %+v", resp)
		}
	})

	t.Run("other kind noop", func(t *testing.T) {
		req := request(t, admissionv1.Create, raw)
		req.Kind.Kind = "Deployment"
		req.Kind.Group = "apps"

		resp := h.Handle(ctx, req)
		if !resp.Allowed || len(resp.Patches) != 0 {
			t.Fatalf("resp = %+v", resp)
		}
	})

	t.Run("unlabeled no patch", func(t *testing.T) {
		p := newPod(nil)
		p.Labels = nil

		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}

		resp := h.Handle(ctx, request(t, admissionv1.Create, b))
		if !resp.Allowed || len(resp.Patches) != 0 {
			t.Fatalf("resp = %+v", resp)
		}
	})
}
