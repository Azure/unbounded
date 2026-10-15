// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

const (
	dataplaneName          = workload.DataplaneDaemonSetName
	dataplaneConfigName    = "racer-dataplane-config"
	dataplaneImageName     = "racer-dataplane"
	dataplaneContainerName = "dataplane"
	dataplaneConfigHash    = "unbounded-cloud.io/racer-dataplane-config-hash"
)

// dataplaneObjects lists the dataplane resources the operator owns. They are
// installation evidence like the controller manifests: a standalone copy is
// never adopted.
func dataplaneObjects(env *component.Env) []client.Object {
	return []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dataplaneConfigName, Namespace: env.Namespace}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}},
	}
}

// controlURL is the in-cluster HTTPS origin of the controller Service.
func controlURL(env *component.Env) string {
	return "https://" + controllerName + "." + env.Namespace + ".svc:8443"
}

// dataplaneWorkloadConfig combines operator-owned identity and wiring with the
// workload inputs administrators may tune in racer-config.
func dataplaneWorkloadConfig(env *component.Env, marker, config *corev1.ConfigMap) (workload.Config, error) {
	owned := map[string]string{
		"POD_NAMESPACE":                   env.Namespace,
		"RACER_CLUSTER_ID":                marker.Data["cluster"],
		"RACER_CONTROL_URL":               controlURL(env),
		"RACER_DATAPLANE_IMAGE":           env.Config.Image(dataplaneImageName),
		"RACER_BOOTSTRAP_TRUST_CONFIGMAP": trustName,
		"RACER_DATAPLANE_SERVICE_ACCOUNT": dataplaneName,
		"RACER_DAEMONSET_NAME":            dataplaneName,
	}

	cfg, err := workload.ConfigFromLookup(func(key string) (string, bool) {
		if value, ok := owned[key]; ok {
			return value, true
		}

		value, ok := config.Data[key]

		return value, ok
	})
	if err != nil {
		return workload.Config{}, fmt.Errorf("racer dataplane workload configuration: %w", err)
	}

	// The controller authenticates only Pods controlled by the single
	// configured DaemonSet, so a pod-network split cannot enroll.
	if len(cfg.PodNetworkNodes) != 0 {
		return workload.Config{}, fmt.Errorf("RACER_POD_NETWORK_NODES is not supported by managed installations: %w", wire.InvalidRequest)
	}

	return cfg, nil
}

// planDataplane adds the dataplane ServiceAccount, tuning ConfigMap, and
// DaemonSet. The DaemonSet waits for every earlier runtime operation, including
// the bootstrap trust ConfigMap it mounts.
func planDataplane(ctx context.Context, env *component.Env, plan *component.Plan, marker, config *corev1.ConfigMap) error {
	cfg, err := dataplaneWorkloadConfig(env, marker, config)
	if err != nil {
		return err
	}

	account := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace},
		AutomountServiceAccountToken: ptr.To(false),
	}

	op, err := ownedRuntimeOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(account), Component: name}, marker.UID)
	if err != nil {
		return err
	}

	plan.Add(op)

	decoded, err := env.DecodeManifestFiles(manifests.Manifests, []string{"dataplane-config.yaml"}, nil)
	if err != nil {
		return err
	}

	if len(decoded) != 1 {
		return fmt.Errorf("racer dataplane config manifest must contain one object")
	}

	defaults := &corev1.ConfigMap{}
	if err := env.Scheme.Convert(decoded[0], defaults, nil); err != nil {
		return err
	}

	bindRuntime(defaults, marker.UID)

	// Every key is administrator payload; the operator only creates defaults.
	tuning, err := preservedConfig(ctx, env, plan, defaults, nil)
	if err != nil {
		return err
	}

	daemonSets, err := workload.DesiredDaemonSets(cfg)
	if err != nil {
		return err
	}

	var dependencies []component.ObjectRef
	for _, dependency := range plan.Operations {
		dependencies = append(dependencies, dependency.Ref())
	}

	hash := component.ConfigMapPayloadHash(tuning)

	for _, ds := range daemonSets {
		for i := range ds.Spec.Template.Spec.Containers {
			container := &ds.Spec.Template.Spec.Containers[i]
			if container.Name == dataplaneContainerName {
				// Explicit env wiring wins over these imports.
				container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfigName}}}}
			}
		}

		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}

		ds.Spec.Template.Annotations[dataplaneConfigHash] = hash

		op := component.Operation{
			Kind: component.OpApply, Object: component.ToUnstructured(ds), Component: name,
			Overridable: true, ValidateOverride: ValidateDataplaneOverride,
			DependsOn: append([]component.ObjectRef(nil), dependencies...),
		}

		op, err = ownedRuntimeOperation(ctx, env, op, marker.UID)
		if err != nil {
			return err
		}

		plan.Add(op)
	}

	return nil
}

// ValidateDataplaneOverride permits scheduling, rollout, resource, and extra
// environment tuning, but keeps identity and trust wiring canonical.
func ValidateDataplaneOverride(original, candidate *unstructured.Unstructured) error {
	container := func(ds *unstructured.Unstructured) (map[string]any, int) {
		var found map[string]any

		count := 0

		containers, _, err := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return nil, 0
		}

		for _, entry := range containers {
			c, ok := entry.(map[string]any)
			if ok && c["name"] == dataplaneContainerName {
				found = c
				count++
			}
		}

		return found, count
	}

	before, beforeCount := container(original)

	after, afterCount := container(candidate)
	if beforeCount != 1 || afterCount != 1 {
		return errors.New("racer DaemonSet must retain exactly one canonical dataplane container")
	}

	if !reflect.DeepEqual(before["envFrom"], after["envFrom"]) {
		return errors.New("racer DaemonSet container dataplane envFrom must match the canonical configuration; tune ConfigMap racer-dataplane-config instead")
	}

	afterEnv := map[string]any{}

	envs, _ := after["env"].([]any)
	for _, entry := range envs {
		if e, ok := entry.(map[string]any); ok {
			afterEnv[fmt.Sprint(e["name"])] = e
		}
	}

	envs, _ = before["env"].([]any)
	for _, entry := range envs {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		if !reflect.DeepEqual(afterEnv[fmt.Sprint(e["name"])], e) {
			return fmt.Errorf("racer DaemonSet container dataplane env %v is operator-owned wiring and cannot be overridden", e["name"])
		}
	}

	for _, field := range []string{"serviceAccountName", "volumes"} {
		beforeValue, _, _ := unstructured.NestedFieldNoCopy(original.Object, "spec", "template", "spec", field)

		afterValue, _, _ := unstructured.NestedFieldNoCopy(candidate.Object, "spec", "template", "spec", field)
		if !reflect.DeepEqual(beforeValue, afterValue) {
			return fmt.Errorf("racer DaemonSet pod %s is operator-owned wiring and cannot be overridden", field)
		}
	}

	return nil
}
