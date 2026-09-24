// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestGantryBackendAuthority(t *testing.T) {
	ds := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: "gantry"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "gantry", Args: []string{"agent", "--config=/etc/gantry/config.yaml", "--content-backend=direct"}}}}}}}

	for _, tc := range []struct {
		name   string
		change func(*corev1.Container)
		valid  bool
	}{
		{name: "resources and unrelated flags", valid: true, change: func(c *corev1.Container) { c.Args = append(c.Args, "--log-level=debug") }},
		{name: "backend flag", change: func(c *corev1.Container) { c.Args = append(c.Args, "--content-backend=racer") }},
		{name: "cache flag", change: func(c *corev1.Container) { c.Args = append(c.Args, "--racer-cache-name", "other") }},
		{name: "cache flag equals", change: func(c *corev1.Container) { c.Args = append(c.Args, "--racer-cache-name=other") }},
		{name: "config redirect", change: func(c *corev1.Container) { c.Args = []string{"agent", "--config=/other"} }},
		{name: "backend env", change: func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "GANTRY_CONTENT_BACKEND", Value: "racer"}} }},
		{name: "cache env", change: func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "GANTRY_RACER_CACHE_NAME", Value: "other"}} }},
		{name: "opaque envFrom", change: func(c *corev1.Container) {
			c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "hidden"}}}}
		}},
		{name: "command redirect", change: func(c *corev1.Container) { c.Command = []string{"custom"} }},
		{name: "positional bypass", change: func(c *corev1.Container) { c.Args = append([]string{"agent", "ignored"}, c.Args[1:]...) }},
		{name: "stop flags bypass", change: func(c *corev1.Container) { c.Args = append([]string{"agent", "--"}, c.Args[1:]...) }},
		{name: "backend flag removed", change: func(c *corev1.Container) { c.Args = c.Args[:2] }},
		{name: "config file shadow", change: func(c *corev1.Container) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "shadow", MountPath: "/etc/gantry/config.yaml"})
		}},
		{name: "config parent shadow", change: func(c *corev1.Container) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "shadow", MountPath: "/etc"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ds.DeepCopy()
			tc.change(&candidate.Spec.Template.Spec.Containers[0])

			err := validateGantryConfig(component.ToUnstructured(ds), component.ToUnstructured(candidate))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestGantryRacerPodSelectionProtected(t *testing.T) {
	no := false
	ds := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"unbounded-cloud.io/gantry-cache-uid": "selected-uid"}},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &no,
			SecurityContext:              &corev1.PodSecurityContext{SupplementalGroups: []int64{65532}},
			Volumes:                      []corev1.Volume{{Name: "racer-sockets"}},
			InitContainers:               []corev1.Container{{Name: "chown-hostpaths", Command: []string{"sh", "-c", "mkdir -p /run/racer/selected-uid"}}},
			Containers:                   []corev1.Container{{Name: "gantry", Args: []string{"agent", "--config=/etc/gantry/config.yaml", "--content-backend=racer", "--racer-cache-name=selected"}, VolumeMounts: []corev1.VolumeMount{{Name: "racer-sockets", MountPath: "/run/racer"}}}},
		},
	}}}

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodTemplateSpec)
	}{
		{name: "UID", mutate: func(p *corev1.PodTemplateSpec) { p.Annotations["unbounded-cloud.io/gantry-cache-uid"] = "other" }},
		{name: "token", mutate: func(p *corev1.PodTemplateSpec) { p.Spec.AutomountServiceAccountToken = nil }},
		{name: "groups", mutate: func(p *corev1.PodTemplateSpec) { p.Spec.SecurityContext = nil }},
		{name: "init", mutate: func(p *corev1.PodTemplateSpec) { p.Spec.InitContainers = nil }},
		{name: "ports", mutate: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "transfer", ContainerPort: 5001}}
		}},
		{name: "mount", mutate: func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].VolumeMounts = nil }},
		{name: "cache args", mutate: func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Args[3] = "--racer-cache-name=other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ds.DeepCopy()
			tc.mutate(&candidate.Spec.Template)

			if err := validateGantryConfig(component.ToUnstructured(ds), component.ToUnstructured(candidate)); err == nil {
				t.Fatal("accepted change to operator-owned backend wiring")
			}
		})
	}
}

func TestGantryOverrideWithholdsHiddenBackend(t *testing.T) {
	ds := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: "gantry"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "gantry", Args: []string{"agent", "--config=/etc/gantry/config.yaml"}}}}}}}
	plan := planWith(component.ToUnstructured(ds), "gantry", "")
	entries := entriesFrom(t, doc(`  - component: gantry
    kind: DaemonSet
    extraArgs:
      gantry: [--content-backend=racer]
`))

	report := Apply(plan, entries, nil)
	if !report.Failed() || len(report.Withheld) != 1 || plan.Len() != 0 {
		t.Fatalf("hidden backend must withhold workload: %#v", report)
	}
}

func TestRacerOriginCoverageOverrides(t *testing.T) {
	ds := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: "racer-edge"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dataplane", VolumeMounts: []corev1.VolumeMount{{Name: "sockets", MountPath: "/run/racer"}}}}}}}}

	for _, tc := range []struct {
		name   string
		change func(*corev1.PodSpec)
		valid  bool
	}{
		{name: "image", valid: true, change: func(p *corev1.PodSpec) { p.Containers[0].Image = "racer:test" }},
		{name: "tuning", valid: true, change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_IO_WORKERS", Value: "2"}, {Name: "RACER_COMPUTE_WORKERS", Value: "2"}, {Name: "RACER_SHARDS", Value: "8"}, {Name: "RACER_BUFFERS_PER_NODE", Value: "16"}}
		}},
		{name: "zero tuning", change: func(p *corev1.PodSpec) { p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_IO_WORKERS", Value: "0"}} }},
		{name: "undersized pool", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_BUFFERS_PER_NODE", Value: "3"}}
		}},
		{name: "duplicate tuning", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_IO_WORKERS", Value: "2"}, {Name: "RACER_IO_WORKERS", Value: "3"}}
		}},
		{name: "indirect tuning", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_IO_WORKERS", Value: "2", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}
		}},
		{name: "launcher", change: func(p *corev1.PodSpec) { p.Containers[0].Args = []string{"custom"} }},
		{name: "storage path", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_SLAB_PATH", Value: "/other"}}
		}},
		{name: "fresh cache path", valid: true, change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_SLAB_PATH", Value: "/cache/cache-tuned-v1.slab"}}
		}},
		{name: "cache traversal", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_SLAB_PATH", Value: "/cache/../other.slab"}}
		}},
		{name: "cache subdirectory", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_SLAB_PATH", Value: "/cache/sub/cache.slab"}}
		}},
		{name: "startup allowance", valid: true, change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_STARTUP_SECONDS", Value: "600"}}
		}},
		{name: "socket env", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_SOCKET_DIR", Value: "/other"}}
		}},
		{name: "opaque env", change: func(p *corev1.PodSpec) { p.Containers[0].EnvFrom = []corev1.EnvFromSource{{Prefix: "RACER_"}} }},
		{name: "narrowed scheduling", change: func(p *corev1.PodSpec) { p.NodeSelector = map[string]string{"subset": "true"} }},
		{name: "socket remount", change: func(p *corev1.PodSpec) { p.Containers[0].VolumeMounts[0].MountPath = "/different" }},
		{name: "identity override", change: func(p *corev1.PodSpec) {
			p.Containers[0].Env = []corev1.EnvVar{{Name: "RACER_UNIVERSE", Value: "other"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ds.DeepCopy()
			tc.change(&candidate.Spec.Template.Spec)

			err := validateRacerOriginCoverage(component.ToUnstructured(ds), component.ToUnstructured(candidate))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
