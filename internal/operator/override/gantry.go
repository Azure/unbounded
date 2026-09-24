// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Backend selection must remain visible to the singleton planner. An override
// cannot redirect the configuration or select a different backend at runtime.
func validateGantryConfig(original, candidate *unstructured.Unstructured) error {
	before, after := &appsv1.DaemonSet{}, &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(original.Object, before); err != nil {
		return err
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(candidate.Object, after); err != nil {
		return err
	}

	var oldContainer corev1.Container

	for _, c := range before.Spec.Template.Spec.Containers {
		if c.Name == "gantry" {
			oldContainer = c
		}
	}
	// Synthetic workloads in generic override tooling may not have the managed
	// Gantry container at all.
	if oldContainer.Name == "" {
		return nil
	}

	found := false

	for _, c := range after.Spec.Template.Spec.Containers {
		if c.Name != "gantry" {
			continue
		}

		found = true

		// Keep the generated prefix intact: inserting a positional argument or
		// '--' before the flags makes Go's flag parser stop before selection.
		if !reflect.DeepEqual(c.Command, oldContainer.Command) || len(c.Args) < len(oldContainer.Args) || !slices.Equal(c.Args[:len(oldContainer.Args)], oldContainer.Args) || !reflect.DeepEqual(configArgs(c.Args), configArgs(oldContainer.Args)) || !reflect.DeepEqual(c.EnvFrom, oldContainer.EnvFrom) {
			return fmt.Errorf("gantry backend selection is operator-owned from ClusterCache annotations; command, generated args, config/backend flags and envFrom cannot redirect it")
		}

		for _, env := range c.Env {
			if env.Name == "GANTRY_CONTENT_BACKEND" || env.Name == "GANTRY_RACER_CACHE_UID" {
				return fmt.Errorf("gantry backend selection is operator-owned from ClusterCache annotations, not %s", env.Name)
			}
		}

		if !reflect.DeepEqual(configMounts(c.VolumeMounts), configMounts(oldContainer.VolumeMounts)) {
			return fmt.Errorf("gantry config mount is owned by gantry-config config.yaml")
		}
	}

	if !found {
		return fmt.Errorf("gantry container is required")
	}

	oldPod, newPod := before.Spec.Template.Spec, after.Spec.Template.Spec
	if !reflect.DeepEqual(oldPod.AutomountServiceAccountToken, newPod.AutomountServiceAccountToken) {
		return fmt.Errorf("gantry service-account token selection is operator-owned")
	}

	if before.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-uid"] != after.Spec.Template.Annotations["unbounded-cloud.io/gantry-cache-uid"] {
		return fmt.Errorf("gantry cache UID rollout annotation is operator-owned")
	}

	if !reflect.DeepEqual(namedVolume(oldPod.Volumes, "config"), namedVolume(newPod.Volumes, "config")) {
		return fmt.Errorf("gantry config volume is owned by gantry-config config.yaml")
	}

	if namedVolume(oldPod.Volumes, "racer-sockets") != nil {
		if !reflect.DeepEqual(oldPod.NodeSelector, newPod.NodeSelector) || !reflect.DeepEqual(oldPod.Affinity, newPod.Affinity) || !reflect.DeepEqual(oldPod.Tolerations, newPod.Tolerations) || !reflect.DeepEqual(oldPod.Volumes, newPod.Volumes) || !reflect.DeepEqual(oldPod.SecurityContext, newPod.SecurityContext) || !reflect.DeepEqual(oldPod.InitContainers, newPod.InitContainers) {
			return fmt.Errorf("gantry Racer scheduling, socket volumes, groups and initialization are operator-owned to guarantee origin coverage")
		}

		for _, c := range newPod.Containers {
			if c.Name == "gantry" && (!reflect.DeepEqual(c.VolumeMounts, oldContainer.VolumeMounts) || !reflect.DeepEqual(c.SecurityContext, oldContainer.SecurityContext) || !reflect.DeepEqual(c.Ports, oldContainer.Ports)) {
				return fmt.Errorf("gantry Racer socket mounts and security context are operator-owned")
			}
		}
	}

	return nil
}

func gantryUsesRacer(plan *component.Plan) bool {
	for _, op := range plan.Operations {
		if op.Component != "gantry" || op.Object.GetKind() != "DaemonSet" || op.Kind == component.OpDelete {
			continue
		}

		volumes, _, err := unstructured.NestedSlice(op.Object.Object, "spec", "template", "spec", "volumes")
		if err != nil {
			continue
		}

		for _, raw := range volumes {
			if volume, ok := raw.(map[string]any); ok && volume["name"] == "racer-sockets" {
				return true
			}
		}
	}

	return false
}

func validateRacerOriginCoverage(original, candidate *unstructured.Unstructured) error {
	for _, field := range []string{"nodeSelector", "affinity", "tolerations", "volumes", "securityContext"} {
		oldValue, _, err := unstructured.NestedFieldNoCopy(original.Object, "spec", "template", "spec", field)
		if err != nil {
			return err
		}

		newValue, _, err := unstructured.NestedFieldNoCopy(candidate.Object, "spec", "template", "spec", field)
		if err != nil {
			return err
		}

		if !reflect.DeepEqual(oldValue, newValue) {
			return fmt.Errorf("racer %s cannot be overridden while Gantry uses Racer; every Gantry node requires its local Racer dataplane and origin", field)
		}
	}

	before, after := &appsv1.DaemonSet{}, &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(original.Object, before); err != nil {
		return err
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(candidate.Object, after); err != nil {
		return err
	}

	oldContainers := append(before.Spec.Template.Spec.Containers, before.Spec.Template.Spec.InitContainers...)
	newContainers := append(after.Spec.Template.Spec.Containers, after.Spec.Template.Spec.InitContainers...)

	for _, old := range oldContainers {
		found := false

		for _, next := range newContainers {
			if old.Name != next.Name {
				continue
			}

			found = true

			envEqual := reflect.DeepEqual(old.Env, next.Env)
			if old.Name == "dataplane" {
				oldEnv, oldErr := racerProtectedEnv(old.Env)
				nextEnv, nextErr := racerProtectedEnv(next.Env)
				envEqual = oldErr == nil && nextErr == nil && reflect.DeepEqual(oldEnv, nextEnv)
			}

			if !reflect.DeepEqual(old.Command, next.Command) || !reflect.DeepEqual(old.Args, next.Args) || !envEqual || !reflect.DeepEqual(old.EnvFrom, next.EnvFrom) || !reflect.DeepEqual(old.VolumeMounts, next.VolumeMounts) || !reflect.DeepEqual(old.SecurityContext, next.SecurityContext) {
				return fmt.Errorf("racer container %q identity, socket mounts and startup are operator-owned while Gantry uses Racer", old.Name)
			}
		}

		if !found {
			return fmt.Errorf("racer container %q is required while Gantry uses Racer", old.Name)
		}
	}

	return nil
}

// Only literal positive tuning counts and a filename inside the managed cache
// directory can vary. Identity, config, sockets, environment sources and the
// operator-owned launcher stay protected.
func racerProtectedEnv(env []corev1.EnvVar) ([]corev1.EnvVar, error) {
	var protected []corev1.EnvVar

	seen := map[string]bool{}
	for _, value := range env {
		if seen[value.Name] {
			return nil, fmt.Errorf("duplicate Racer env %s", value.Name)
		}

		seen[value.Name] = true
		switch value.Name {
		case "RACER_IO_WORKERS", "RACER_COMPUTE_WORKERS", "RACER_SHARDS", "RACER_BUFFERS_PER_NODE", "RACER_STARTUP_SECONDS":
			n, err := strconv.ParseUint(value.Value, 10, 32)
			if err != nil || n == 0 || value.ValueFrom != nil || (value.Name == "RACER_BUFFERS_PER_NODE" && n < 4) {
				return nil, fmt.Errorf("invalid Racer tuning env %s", value.Name)
			}
		case "RACER_SLAB_PATH":
			if value.ValueFrom != nil || path.Clean(value.Value) != value.Value || path.Dir(value.Value) != "/cache" || !strings.HasSuffix(path.Base(value.Value), ".slab") || path.Base(value.Value) == ".slab" || strings.ContainsAny(value.Value, "\x00\r\n") {
				return nil, fmt.Errorf("racer slab path must be a literal /cache/<filename>.slab")
			}
		default:
			protected = append(protected, value)
		}
	}

	return protected, nil
}

func configArgs(args []string) []string {
	var selected []string

	for i, arg := range args {
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		if name == "config" || name == "content-backend" || name == "racer-cache-uid" {
			selected = append(selected, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				selected = append(selected, args[i+1])
			}
		}
	}

	return selected
}

// A second volume mounted over the config file (or any parent directory) can
// redirect the source without changing the named config mount itself.
func configMounts(mounts []corev1.VolumeMount) []corev1.VolumeMount {
	var selected []corev1.VolumeMount

	for _, mount := range mounts {
		p := path.Clean(mount.MountPath)
		if mount.Name == "config" || p == "/" || p == "/etc" || p == "/etc/gantry" || strings.HasPrefix(p, "/etc/gantry/") {
			selected = append(selected, mount)
		}
	}

	return selected
}

func namedVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for _, volume := range volumes {
		if volume.Name == name {
			return &volume
		}
	}

	return nil
}
